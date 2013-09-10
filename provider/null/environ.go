// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package null

import (
	"errors"
	"fmt"
	"sync"

	"launchpad.net/juju-core/constraints"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/cloudinit"
	"launchpad.net/juju-core/environs/config"
	"launchpad.net/juju-core/environs/localstorage"
	"launchpad.net/juju-core/environs/manual"
	"launchpad.net/juju-core/environs/sshstorage"
	"launchpad.net/juju-core/instance"
	"launchpad.net/juju-core/provider"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api"
	"launchpad.net/juju-core/tools"
)

type nullEnviron struct {
	cfg      *environConfig
	cfgmutex sync.Mutex
}

var errNoStartInstance = errors.New("null provider cannot start instances")
var errNoStopInstance = errors.New("null provider cannot stop instances")
var errNoOpenPorts = errors.New("null provider cannot open ports")
var errNoClosePorts = errors.New("null provider cannot close ports")

func (_ *nullEnviron) StartInstance(_ constraints.Value, _ tools.List, _ *cloudinit.MachineConfig) (instance.Instance, *instance.HardwareCharacteristics, error) {
	return nil, nil, errNoStartInstance
}

func (_ *nullEnviron) StopInstances([]instance.Instance) error {
	return errNoStopInstance
}

func (e *nullEnviron) AllInstances() ([]instance.Instance, error) {
	return []instance.Instance{nullBootstrapInstance{}}, nil
}

func (e *nullEnviron) envConfig() (cfg *environConfig) {
	e.cfgmutex.Lock()
	cfg = e.cfg
	e.cfgmutex.Unlock()
	return cfg
}

func (e *nullEnviron) Config() *config.Config {
	return e.envConfig().Config
}

func (e *nullEnviron) Name() string {
	return e.envConfig().Name()
}

func (e *nullEnviron) Bootstrap(_ constraints.Value, possibleTools tools.List, machineID string) error {
	return manual.Bootstrap(manual.BootstrapArgs{
		Host:          e.envConfig().sshHost(),
		Environ:       e,
		MachineId:     machineID,
		PossibleTools: possibleTools,
	})
}

func (e *nullEnviron) StateInfo() (*state.Info, *api.Info, error) {
	return provider.StateInfo(e)
}

func (e *nullEnviron) SetConfig(cfg *config.Config) error {
	e.cfgmutex.Lock()
	defer e.cfgmutex.Unlock()
	envConfig, err := nullProvider{}.validate(cfg, e.cfg.Config)
	if err != nil {
		return err
	}
	e.cfg = envConfig
	return nil
}

func (e *nullEnviron) Instances(ids []instance.Id) (instances []instance.Instance, err error) {
	instances = make([]instance.Instance, len(ids))
	var found bool
	for i, id := range ids {
		if id == manual.BootstrapInstanceId {
			instances[i] = nullBootstrapInstance{}
			found = true
		}
	}
	if !found {
		err = environs.ErrNoInstances
	}
	return instances, err
}

// Implements environs/bootstrap.BootstrapStorage.
func (e *nullEnviron) BootstrapStorage() (environs.Storage, error) {
	cfg := e.envConfig()
	return sshstorage.NewSSHStorage(cfg.sshHost(), cfg.storageDir())
}

func (e *nullEnviron) Storage() environs.Storage {
	return localstorage.Client(e.envConfig().storageAddr())
}

func (e *nullEnviron) PublicStorage() environs.StorageReader {
	return environs.EmptyStorage
}

func (e *nullEnviron) Destroy(insts []instance.Instance) error {
	if len(insts) > 0 {
		return fmt.Errorf("null provider cannot destroy instances: %v", insts)
	}
	return nil
}

func (e *nullEnviron) OpenPorts(ports []instance.Port) error {
	return errNoOpenPorts
}

func (e *nullEnviron) ClosePorts(ports []instance.Port) error {
	return errNoClosePorts
}

func (e *nullEnviron) Ports() ([]instance.Port, error) {
	return []instance.Port{}, nil
}

func (_ *nullEnviron) Provider() environs.EnvironProvider {
	return nullProvider{}
}

func (e *nullEnviron) StorageAddr() string {
	return e.envConfig().storageListenAddr()
}

func (e *nullEnviron) StorageDir() string {
	return e.envConfig().storageDir()
}

func (e *nullEnviron) SharedStorageAddr() string {
    return ""
}

func (e *nullEnviron) SharedStorageDir() string {
    return ""
}
