package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/events"
	"github.com/pelican/wings/remote"
)

// hookClient returns a fixed installation script. Calls to any other method of
// the embedded nil remote.Client panic.
type hookClient struct {
	remote.Client
	script remote.InstallationScript
}

func (c *hookClient) GetInstallationScript(context.Context, string) (remote.InstallationScript, error) {
	return c.script, nil
}

// hookInstaller records how it was called and returns err.
type hookInstaller struct {
	err        error
	calls      int
	script     *remote.InstallationScript
	installing bool
}

func (i *hookInstaller) Run(s *Server, script *remote.InstallationScript) error {
	i.calls++
	i.script = script
	i.installing = s.IsInstalling()
	return i.err
}

// hookEnvironment implements only the methods InitServer and
// SyncWithEnvironment call, plus ImageAndStopConfigurable. Calls to any other
// method of the embedded nil environment.ProcessEnvironment panic.
type hookEnvironment struct {
	environment.ProcessEnvironment
	cfg   *environment.Configuration
	bus   *events.Bus
	image string
	stop  remote.ProcessStopConfiguration
}

func (e *hookEnvironment) Config() *environment.Configuration { return e.cfg }
func (e *hookEnvironment) Events() *events.Bus                { return e.bus }
func (e *hookEnvironment) SetLogCallback(func([]byte))        {}
func (e *hookEnvironment) InSituUpdate() error                { return nil }
func (e *hookEnvironment) SetImage(i string)                  { e.image = i }
func (e *hookEnvironment) SetStopConfiguration(c remote.ProcessStopConfiguration) {
	e.stop = c
}

// setHookConfig sets a global configuration that InitServer can run with.
func setHookConfig(t *testing.T) {
	config.Set(&config.Configuration{
		AuthenticationToken: "test",
		System:              config.SystemConfiguration{Data: t.TempDir()},
	})
}

// hookServerSettings returns the minimal Panel settings InitServer needs.
func hookServerSettings(uuid string) json.RawMessage {
	return json.RawMessage(`{"uuid":"` + uuid + `","allocations":{"default":{"ip":"0.0.0.0","port":25565}}}`)
}

func TestInternalInstallRunsInstallerUnderLock(t *testing.T) {
	client := &hookClient{script: remote.InstallationScript{ContainerImage: "installer:latest"}}
	s, err := New(client)
	if err != nil {
		t.Fatalf("New() returned unexpected error: %v", err)
	}
	installer := &hookInstaller{}
	s.installer = installer

	if err := s.internalInstall(); err != nil {
		t.Fatalf("internalInstall() returned unexpected error: %v", err)
	}
	if installer.calls != 1 {
		t.Fatalf("installer called %d times, expected 1", installer.calls)
	}
	if installer.script == nil || installer.script.ContainerImage != "installer:latest" {
		t.Fatalf("installer received script %+v, expected the Panel's script", installer.script)
	}
	if !installer.installing {
		t.Fatal("installer ran without the installation lock held")
	}
	if s.IsInstalling() {
		t.Fatal("installation lock still held after internalInstall() returned")
	}
}

func TestInternalInstallReturnsInstallerError(t *testing.T) {
	s, err := New(&hookClient{})
	if err != nil {
		t.Fatalf("New() returned unexpected error: %v", err)
	}
	installErr := errors.New("script failed")
	s.installer = &hookInstaller{err: installErr}

	if err := s.internalInstall(); !errors.Is(err, installErr) {
		t.Fatalf("internalInstall() error = %v, expected %v", err, installErr)
	}
	if s.IsInstalling() {
		t.Fatal("installation lock still held after a failed install")
	}
}

func TestInternalInstallRefusesWhileInstalling(t *testing.T) {
	s, err := New(&hookClient{})
	if err != nil {
		t.Fatalf("New() returned unexpected error: %v", err)
	}
	installer := &hookInstaller{}
	s.installer = installer
	s.installing.Store(true)

	if err := s.internalInstall(); err == nil {
		t.Fatal("internalInstall() returned nil while another install held the lock")
	}
	if installer.calls != 0 {
		t.Fatalf("installer called %d times while the lock was held, expected 0", installer.calls)
	}
}

func TestInitServerUsesManagerHooks(t *testing.T) {
	setHookConfig(t)

	installer := &hookInstaller{}
	var factoryCalls int
	env := &hookEnvironment{bus: events.NewBus()}
	factory := func(s *Server, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
		factoryCalls++
		env.cfg = cfg
		return env, nil
	}
	m := NewEmptyManager(nil, WithEnvironmentFactory(factory), WithInstaller(installer))

	s, err := m.InitServer(remote.ServerConfigurationResponse{
		Settings:             hookServerSettings("2d9b0c7e-0000-4000-8000-000000000000"),
		ProcessConfiguration: &remote.ProcessConfiguration{},
	})
	if err != nil {
		t.Fatalf("InitServer() returned unexpected error: %v", err)
	}
	defer s.CtxCancel()

	if factoryCalls != 1 {
		t.Fatalf("environment factory called %d times, expected 1", factoryCalls)
	}
	if s.Environment != env {
		t.Fatal("server environment is not the one returned by the factory")
	}
	if s.installer != installer {
		t.Fatal("server installer is not the one set on the manager")
	}
}

func TestInitServerFactoryError(t *testing.T) {
	setHookConfig(t)

	factoryErr := errors.New("no runtime")
	factory := func(*Server, *environment.Configuration) (environment.ProcessEnvironment, error) {
		return nil, factoryErr
	}
	m := NewEmptyManager(nil, WithEnvironmentFactory(factory))

	_, err := m.InitServer(remote.ServerConfigurationResponse{
		Settings:             hookServerSettings("2d9b0c7e-0000-4000-8000-000000000001"),
		ProcessConfiguration: &remote.ProcessConfiguration{},
	})
	if !errors.Is(err, factoryErr) {
		t.Fatalf("InitServer() error = %v, expected %v", err, factoryErr)
	}
}

func TestSyncWithEnvironmentConfiguresImageAndStop(t *testing.T) {
	setHookConfig(t)

	env := &hookEnvironment{bus: events.NewBus()}
	factory := func(_ *Server, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
		env.cfg = cfg
		return env, nil
	}
	m := NewEmptyManager(nil, WithEnvironmentFactory(factory))
	s, err := m.InitServer(remote.ServerConfigurationResponse{
		Settings: hookServerSettings("2d9b0c7e-0000-4000-8000-000000000002"),
		ProcessConfiguration: &remote.ProcessConfiguration{
			Stop: remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"},
		},
	})
	if err != nil {
		t.Fatalf("InitServer() returned unexpected error: %v", err)
	}
	defer s.CtxCancel()
	s.cfg.Container.Image = "game:1"

	s.SyncWithEnvironment()

	if env.image != "game:1" {
		t.Fatalf("environment image = %q, expected %q", env.image, "game:1")
	}
	if env.stop.Type != remote.ProcessStopCommand || env.stop.Value != "stop" {
		t.Fatalf("environment stop configuration = %+v, expected the server's", env.stop)
	}
}
