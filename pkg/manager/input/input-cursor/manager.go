// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package cursor

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/urso/sderr"

	"github.com/elastic/go-concert/unison"

	"github.com/elastic/elastic-agent-inputs/pkg/manager/input"
	"github.com/elastic/elastic-agent-inputs/pkg/statestore"
	conf "github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
)

// InputManager is used to create, manage, and coordinate stateful inputs and
// their persistent state.
// The InputManager ensures that only one input can be active for a unique source.
// If two inputs have overlapping sources, both can still collect data, but
// only one input will collect from the common source.
//
// The InputManager automatically cleans up old entries without an active
// input, and without any pending update operations for the persistent store.
//
// The Type field is used to create the key name in the persistent store. Users
// are allowed to add a custome per input configuration ID using the `id`
// setting, to collect the same source multiple times, but with different
// state. The key name in the persistent store becomes <Type>-[<ID>]-<Source Name>
type InputManager struct {
	Logger *logp.Logger

	// StateStore gives the InputManager access to the persitent key value store.
	StateStore StateStore

	// Type must contain the name of the input type. It is used to create the key name
	// for all sources the inputs collect from.
	Type string

	// DefaultCleanTimeout configures the key/value garbage collection interval.
	// The InputManager will only collect keys for the configured 'Type'
	DefaultCleanTimeout time.Duration

	// Configure returns an array of Sources, and a configured Input instances
	// that will be used to collect events from each source.
	Configure func(cfg *conf.C) ([]Source, Input, error)

	initOnce sync.Once
	initErr  error
	store    *store
}

// Source describe a source the input can collect data from.
// The `Name` method must return an unique name, that will be used to identify
// the source in the persistent state store.
type Source interface {
	Name() string
}

var (
	errNoSourceConfigured = errors.New("no source has been configured")
	errNoInputRunner      = errors.New("no input runner available")
)

// StateStore interface and configurations used to give the Manager access to the persistent store.
type StateStore interface {
	Access() (*statestore.Store, error)
	CleanupInterval() time.Duration
}

func (cim *InputManager) init() error {
	cim.initOnce.Do(func() {
		if cim.DefaultCleanTimeout <= 0 {
			cim.DefaultCleanTimeout = 30 * time.Minute
		}

		log := cim.Logger.With("input_type", cim.Type)
		var store *store
		store, cim.initErr = openStore(log, cim.StateStore, cim.Type)
		if cim.initErr != nil {
			return
		}

		cim.store = store
	})

	return cim.initErr
}

// Init starts background processes for deleting old entries from the
// persistent store if mode is ModeRun.
func (cim *InputManager) Init(group unison.Group, mode input.Mode) error {
	if mode != input.ModeRun {
		return nil
	}

	if err := cim.init(); err != nil {
		return err
	}

	log := cim.Logger.With("input_type", cim.Type)

	store := cim.store
	cleaner := &cleaner{log: log}
	store.Retain()
	err := group.Go(func(canceler context.Context) error {
		defer cim.shutdown()
		defer store.Release()
		interval := cim.StateStore.CleanupInterval()
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		cleaner.run(canceler, store, interval)
		return nil
	})
	if err != nil {
		store.Release()
		cim.shutdown()
		return sderr.Wrap(err, "Can not start registry cleanup process")
	}

	return nil
}

func (cim *InputManager) shutdown() {
	cim.store.Release()
}

// Create builds a new input.Input using the provided Configure function.
// The Input will run a go-routine per source that has been configured.
func (cim *InputManager) Create(config *conf.C) (input.Input, error) {
	if err := cim.init(); err != nil {
		return nil, err
	}

	settings := struct {
		ID           string        `config:"id"`
		CleanTimeout time.Duration `config:"clean_timeout"`
	}{ID: "", CleanTimeout: cim.DefaultCleanTimeout}
	if err := config.Unpack(&settings); err != nil {
		return nil, err
	}

	sources, inp, err := cim.Configure(config)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, errNoSourceConfigured
	}
	if inp == nil {
		return nil, errNoInputRunner
	}

	return &managedInput{
		manager:      cim,
		ID:           settings.ID,
		sources:      sources,
		input:        inp,
		cleanTimeout: settings.CleanTimeout,
	}, nil
}

// Lock locks a key for exclusive access and returns an resource that can be used to modify
// the cursor state and unlock the key.
func (cim *InputManager) lock(ctx input.Context, key string) (*resource, error) {
	resource := cim.store.Get(key)
	err := lockResource(ctx.Logger, resource, ctx.Cancelation)
	if err != nil {
		resource.Release()
		return nil, err
	}
	return resource, nil
}

func lockResource(log *logp.Logger, resource *resource, canceler input.Canceler) error {
	if !resource.lock.TryLock() {
		log.Infof("Resource '%v' currently in use, waiting...", resource.key)
		err := resource.lock.LockContext(canceler)
		if err != nil {
			log.Infof("Input for resource '%v' has been stopped while waiting", resource.key)
			return err
		}
	}
	return nil
}

func releaseResource(resource *resource) {
	resource.lock.Unlock()
	resource.Release()
}
