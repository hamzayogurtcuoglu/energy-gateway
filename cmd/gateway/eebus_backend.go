package main

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	eebusapi "github.com/enbility/eebus-go/api"
	"github.com/enbility/eebus-go/service"
	ucapi "github.com/enbility/eebus-go/usecases/api"
	"github.com/enbility/eebus-go/usecases/eg/lpp"
	"github.com/enbility/eebus-go/usecases/ma/mpc"
	shipapi "github.com/enbility/ship-go/api"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/enbility/spine-go/model"

	"energy-gateway/internal/eebuscert"
)

// eebusBackend controls the EEBUS inverter: it writes the production limit
// (eg/lpp) and reads live meter values (ma/mpc). It also implements the EEBUS
// service reader interface.
type eebusBackend struct {
	logger *slog.Logger
	svc    *service.Service
	uclpp  ucapi.EgLPPInterface
	ucmpc  ucapi.MaMPCInterface

	mu     sync.Mutex
	entity spineapi.EntityRemoteInterface
	lastW  *float64
}

func newEEBUSBackend(logger *slog.Logger, port int, ifaces []string, inverterSKI, certPath, keyPath string) (*eebusBackend, string, error) {
	certificate, ski, err := eebuscert.LoadOrCreate(certPath, keyPath, "EEBUS-Matter-Gateway")
	if err != nil {
		return nil, "", fmt.Errorf("certificate: %w", err)
	}

	configuration, err := eebusapi.NewConfiguration(
		"Demo", "Demo", "Gateway", "555555555",
		model.DeviceTypeTypeElectricitySupplySystem,
		[]model.EntityTypeType{model.EntityTypeTypeGridGuard},
		port, certificate, 4*time.Second,
	)
	if err != nil {
		return nil, "", fmt.Errorf("configuration: %w", err)
	}
	configuration.SetAlternateIdentifier("EEBUS-Matter-Gateway-555555555")
	if len(ifaces) > 0 {
		configuration.SetInterfaces(ifaces)
	}

	b := &eebusBackend{logger: logger}
	svc := service.NewService(configuration, b)
	if err := svc.Setup(); err != nil {
		return nil, "", fmt.Errorf("setup: %w", err)
	}
	b.svc = svc

	localEntity := svc.LocalDevice().EntityForType(model.EntityTypeTypeGridGuard)
	b.uclpp = lpp.NewLPP(localEntity, b.onLPPEvent)
	svc.AddUseCase(b.uclpp)
	b.ucmpc = mpc.NewMPC(localEntity, b.onMPCEvent)
	svc.AddUseCase(b.ucmpc)

	svc.SetAutoAccept(true)
	svc.Start()

	if inverterSKI != "" {
		svc.RegisterRemoteSKI(inverterSKI)
	}
	return b, ski, nil
}

func (b *eebusBackend) shutdown() {
	if b.svc != nil {
		b.svc.Shutdown()
	}
}

func (b *eebusBackend) SetLimit(watts float64, duration time.Duration) error {
	b.mu.Lock()
	entity := b.entity
	b.mu.Unlock()
	if entity == nil {
		return fmt.Errorf("eebus inverter not connected yet")
	}
	if duration <= 0 {
		duration = 2 * time.Minute
	}
	if err := b.writeLimit(entity, ucapi.LoadLimit{IsActive: true, Value: watts, Duration: duration}); err != nil {
		return err
	}
	b.mu.Lock()
	value := watts
	b.lastW = &value
	b.mu.Unlock()
	b.logger.Info("applied production limit over EEBUS", "watts", watts)
	return nil
}

func (b *eebusBackend) Reset() error {
	b.mu.Lock()
	entity := b.entity
	b.mu.Unlock()
	if entity == nil {
		return fmt.Errorf("eebus inverter not connected yet")
	}
	if err := b.writeLimit(entity, ucapi.LoadLimit{IsActive: false, Value: 0}); err != nil {
		return err
	}
	b.mu.Lock()
	b.lastW = nil
	b.mu.Unlock()
	b.logger.Info("cleared production limit over EEBUS")
	return nil
}

func (b *eebusBackend) writeLimit(entity spineapi.EntityRemoteInterface, limit ucapi.LoadLimit) error {
	resultCh := make(chan model.ResultDataType, 1)
	callback := func(msg model.ResultDataType) {
		select {
		case resultCh <- msg:
		default:
		}
	}
	if _, err := b.uclpp.WriteProductionLimit(entity, limit, callback); err != nil {
		return fmt.Errorf("write production limit: %w", err)
	}
	select {
	case msg := <-resultCh:
		if msg.ErrorNumber == nil || *msg.ErrorNumber != model.ErrorNumberTypeNoError {
			description := ""
			if msg.Description != nil {
				description = string(*msg.Description)
			}
			return fmt.Errorf("inverter rejected limit: %s", description)
		}
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timed out waiting for inverter response")
	}
	return nil
}

func (b *eebusBackend) Status() backendStatus {
	b.mu.Lock()
	entity := b.entity
	status := backendStatus{Protocol: "eebus", Connected: entity != nil, LastLimitW: b.lastW}
	b.mu.Unlock()

	if entity != nil {
		if v, err := b.ucmpc.Power(entity); err == nil {
			status.MomentaryPowerW = &v
		}
		if v, err := b.ucmpc.EnergyProduced(entity); err == nil {
			status.EnergyProducedWh = &v
		}
		if vs, err := b.ucmpc.VoltagePerPhase(entity); err == nil && len(vs) > 0 {
			v := vs[0]
			status.VoltageV = &v
		}
		if v, err := b.ucmpc.Frequency(entity); err == nil {
			status.FrequencyHz = &v
		}
	}
	return status
}

func (b *eebusBackend) onLPPEvent(_ string, _ spineapi.DeviceRemoteInterface, entity spineapi.EntityRemoteInterface, event eebusapi.EventType) {
	switch event {
	case lpp.UseCaseSupportUpdate:
		b.logger.Info("inverter supports production limitation (CS-LPP)")
	case lpp.DataUpdateLimit:
		b.mu.Lock()
		b.entity = entity
		b.mu.Unlock()
		b.logger.Info("inverter ready to receive production limits")
	}
}

func (b *eebusBackend) onMPCEvent(_ string, _ spineapi.DeviceRemoteInterface, entity spineapi.EntityRemoteInterface, event eebusapi.EventType) {
	switch event {
	case mpc.UseCaseSupportUpdate:
		b.logger.Info("inverter supports power monitoring (MPC)")
	case mpc.DataUpdatePower, mpc.DataUpdateEnergyProduced, mpc.DataUpdateVoltagePerPhase, mpc.DataUpdateFrequency:
		b.mu.Lock()
		if b.entity == nil {
			b.entity = entity
		}
		b.mu.Unlock()
	}
}

func (b *eebusBackend) RemoteSKIConnected(_ eebusapi.ServiceInterface, ski string) {
	b.logger.Info("connected to eebus inverter", "ski", ski)
}

func (b *eebusBackend) RemoteSKIDisconnected(_ eebusapi.ServiceInterface, ski string) {
	b.mu.Lock()
	b.entity = nil
	b.mu.Unlock()
	b.logger.Info("disconnected from eebus inverter", "ski", ski)
}

func (b *eebusBackend) VisibleRemoteServicesUpdated(_ eebusapi.ServiceInterface, _ []shipapi.RemoteService) {
}

func (b *eebusBackend) ServiceShipIDUpdate(_ string, _ string) {}

func (b *eebusBackend) ServicePairingDetailUpdate(_ string, _ *shipapi.ConnectionStateDetail) {}
