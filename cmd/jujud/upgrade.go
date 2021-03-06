package main

import (
	"fmt"
	"time"

	"github.com/juju/errors"
	"github.com/juju/names"
	"github.com/juju/utils"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/mongo"
	"github.com/juju/juju/state"
	"github.com/juju/juju/state/api"
	"github.com/juju/juju/state/api/params"
	"github.com/juju/juju/upgrades"
	"github.com/juju/juju/version"
	"github.com/juju/juju/worker"
)

type upgradingMachineAgent interface {
	ensureMongoServer(agent.Config) error
	setMachineStatus(*api.State, params.Status, string) error
	CurrentConfig() agent.Config
	ChangeConfig(AgentConfigMutator) error
}

var upgradesPerformUpgrade = upgrades.PerformUpgrade // Allow patching for tests

func NewUpgradeWorkerContext() *upgradeWorkerContext {
	return &upgradeWorkerContext{
		UpgradeComplete: make(chan struct{}),
	}
}

type upgradeWorkerContext struct {
	UpgradeComplete chan struct{}
	agent           upgradingMachineAgent
	apiState        *api.State
	jobs            []params.MachineJob
}

func (c *upgradeWorkerContext) Worker(
	agent upgradingMachineAgent,
	apiState *api.State,
	jobs []params.MachineJob,
) worker.Worker {
	c.agent = agent
	c.apiState = apiState
	c.jobs = jobs
	return worker.NewSimpleWorker(c.run)
}

type apiLostDuringUpgrade struct {
	err error
}

func (e *apiLostDuringUpgrade) Error() string {
	return fmt.Sprintf("API connection lost during upgrade: %v", e.err)
}

func isAPILostDuringUpgrade(err error) bool {
	_, ok := err.(*apiLostDuringUpgrade)
	return ok
}

func (c *upgradeWorkerContext) run(stop <-chan struct{}) error {
	select {
	case <-c.UpgradeComplete:
		// Our work is already done (we're probably being restarted
		// because the API connection has gone down), so do nothing.
		return nil
	default:
	}

	agentConfig := c.agent.CurrentConfig()

	// If the machine agent is a state server, flag that state
	// needs to be opened before running upgrade steps
	needsState := false
	for _, job := range c.jobs {
		if job == params.JobManageEnviron {
			needsState = true
		}
	}
	// We need a *state.State for upgrades. We open it independently
	// of StateWorker, because we have no guarantees about when
	// and how often StateWorker might run.
	var st *state.State
	if needsState {
		if err := c.agent.ensureMongoServer(agentConfig); err != nil {
			return err
		}
		var err error
		info, ok := agentConfig.MongoInfo()
		if !ok {
			return fmt.Errorf("no state info available")
		}
		st, err = state.Open(info, mongo.DialOpts{}, environs.NewStatePolicy())
		if err != nil {
			return err
		}
		defer st.Close()
	}
	if err := c.runUpgrades(st, agentConfig); err != nil {
		// Only return an error from the worker if the connection to
		// state went away (possible mongo master change). Returning
		// an error when the connection is lost will cause the agent
		// to restart.
		//
		// For other errors, the error is not returned because we want
		// the machine agent to stay running in an error state waiting
		// for user intervention.
		if isAPILostDuringUpgrade(err) {
			return err
		}
	} else {
		// Upgrade succeeded - signal that the upgrade is complete.
		close(c.UpgradeComplete)
	}
	return nil
}

// runUpgrades runs the upgrade operations for each job type and
// updates the updatedToVersion on success.
func (c *upgradeWorkerContext) runUpgrades(st *state.State, agentConfig agent.Config) error {
	from := version.Current
	from.Number = agentConfig.UpgradedToVersion()
	if from == version.Current {
		logger.Infof("upgrade to %v already completed.", version.Current)
		return nil
	}

	a := c.agent
	tag := agentConfig.Tag().(names.MachineTag)

	isMaster, err := isMachineMaster(st, tag)
	if err != nil {
		return errors.Trace(err)
	}

	err = a.ChangeConfig(func(agentConfig agent.ConfigSetter) error {
		var upgradeErr error
		a.setMachineStatus(c.apiState, params.StatusStarted,
			fmt.Sprintf("upgrading to %v", version.Current))
		context := upgrades.NewContext(agentConfig, c.apiState, st)
		for _, job := range c.jobs {
			target := upgradeTarget(job, isMaster)
			if target == "" {
				continue
			}
			logger.Infof("starting upgrade from %v to %v for %v %q",
				from, version.Current, target, tag)

			attempts := getUpgradeRetryStrategy()
			for attempt := attempts.Start(); attempt.Next(); {
				upgradeErr = upgradesPerformUpgrade(from.Number, target, context)
				if upgradeErr == nil {
					break
				}
				if connectionIsDead(c.apiState) {
					// API connection has gone away - abort!
					return &apiLostDuringUpgrade{upgradeErr}
				}
				retryText := "will retry"
				if !attempt.HasNext() {
					retryText = "giving up"
				}
				logger.Errorf("upgrade from %v to %v for %v %q failed (%s): %v",
					from, version.Current, target, tag, retryText, upgradeErr)
				a.setMachineStatus(c.apiState, params.StatusError,
					fmt.Sprintf("upgrade to %v failed (%s): %v",
						version.Current, retryText, upgradeErr))
			}
		}
		if upgradeErr != nil {
			return upgradeErr
		}
		agentConfig.SetUpgradedToVersion(version.Current.Number)
		return nil
	})
	if err != nil {
		logger.Errorf("upgrade to %v failed: %v", version.Current, err)
		return err
	}

	logger.Infof("upgrade to %v completed successfully.", version.Current)
	a.setMachineStatus(c.apiState, params.StatusStarted, "")
	return nil
}

func isMachineMaster(st *state.State, tag names.MachineTag) (bool, error) {
	if st == nil {
		// If there is no state, we aren't a master.
		return false, nil
	}
	// Not calling the agent openState method as it does other checks
	// we really don't care about here.  All we need here is the machine
	// so we can determine if we are the master or not.
	machine, err := st.Machine(tag.Id())
	if err != nil {
		// This shouldn't happen, and if it does, the state worker will have
		// found out before us, and already errored, or is likely to error out
		// very shortly.  All we do here is return the error. The state worker
		// returns an error that will cause the agent to be terminated.
		return false, errors.Trace(err)
	}
	isMaster, err := mongo.IsMaster(st.MongoSession(), machine)
	if err != nil {
		return false, errors.Trace(err)
	}
	return isMaster, nil
}

var getUpgradeRetryStrategy = func() utils.AttemptStrategy {
	return utils.AttemptStrategy{
		Delay: 2 * time.Minute,
		Min:   5,
	}
}

func upgradeTarget(job params.MachineJob, isMaster bool) upgrades.Target {
	switch job {
	case params.JobManageEnviron:
		if isMaster {
			return upgrades.DatabaseMaster
		}
		return upgrades.StateServer
	case params.JobHostUnits:
		return upgrades.HostMachine
	}
	return ""
}
