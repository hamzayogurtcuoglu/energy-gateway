// Command eebus-energyguard is a minimal official EEBUS Energy Guard client.
//
// It uses the enbility/eebus-go `eg/lpp` use case (Energy Guard - Limitation of
// Power Production) to connect to an inverter that implements `cs/lpp` and write
// an active power production limit to it over real EEBUS SHIP/SPINE.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	eebusapi "github.com/enbility/eebus-go/api"
	"github.com/enbility/eebus-go/service"
	ucapi "github.com/enbility/eebus-go/usecases/api"
	"github.com/enbility/eebus-go/usecases/eg/lpp"
	shipapi "github.com/enbility/ship-go/api"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/enbility/spine-go/model"

	"eebus-inverter-simulator/internal/eebuscert"
)

type energyGuard struct {
	service     *service.Service
	uclpp       ucapi.EgLPPInterface
	logger      *slog.Logger
	inverterSKI string
	limitW      float64

	mu    sync.Mutex
	wrote bool
}

func main() {
	port := flag.Int("eebus-port", 47712, "EEBUS SHIP listen port for this client")
	ifaces := flag.String("eebus-interface", "", "network interface names for mDNS, e.g. Wi-Fi")
	inverterSKI := flag.String("inverter-ski", "", "SKI of the inverter to control; empty just prints this client's SKI")
	limitW := flag.Float64("limit-w", 3000, "production limit to write, in watts")
	certPath := flag.String("cert", ".energyguard/guard.crt", "TLS certificate path")
	keyPath := flag.String("key", ".energyguard/guard.key", "TLS private key path")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	certificate, ski, err := eebuscert.LoadOrCreate(*certPath, *keyPath, "EnergyGuard-987654321")
	if err != nil {
		fmt.Fprintf(os.Stderr, "certificate error: %v\n", err)
		os.Exit(1)
	}

	configuration, err := eebusapi.NewConfiguration(
		"Demo", "Demo", "EnergyGuard", "987654321",
		model.DeviceTypeTypeElectricitySupplySystem,
		[]model.EntityTypeType{model.EntityTypeTypeGridGuard},
		*port, certificate, 4*time.Second,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(1)
	}
	configuration.SetAlternateIdentifier("Demo-EnergyGuard-987654321")
	if list := splitCSV(*ifaces); len(list) > 0 {
		configuration.SetInterfaces(list)
	}

	guard := &energyGuard{logger: logger, inverterSKI: *inverterSKI, limitW: *limitW}
	guard.service = service.NewService(configuration, guard)
	if err := guard.service.Setup(); err != nil {
		fmt.Fprintf(os.Stderr, "setup error: %v\n", err)
		os.Exit(1)
	}

	localEntity := guard.service.LocalDevice().EntityForType(model.EntityTypeTypeGridGuard)
	guard.uclpp = lpp.NewLPP(localEntity, guard.onLPPEvent)
	guard.service.AddUseCase(guard.uclpp)

	guard.service.SetAutoAccept(true)
	guard.service.Start()
	defer guard.service.Shutdown()

	if *inverterSKI == "" {
		logger.Info("energy guard running with no target; use this SKI on the inverter's -remote-ski", "ski", ski)
	} else {
		guard.service.RegisterRemoteSKI(*inverterSKI)
		logger.Info("energy guard running", "ski", ski, "targetInverterSKI", *inverterSKI, "limitW", *limitW)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	logger.Info("energy guard shutting down")
}

func (h *energyGuard) onLPPEvent(_ string, _ spineapi.DeviceRemoteInterface, entity spineapi.EntityRemoteInterface, event eebusapi.EventType) {
	switch event {
	case lpp.UseCaseSupportUpdate:
		h.logger.Info("inverter supports production limitation (CS-LPP)")
	case lpp.DataUpdateLimit:
		// At this point the remote LoadControl limit descriptions and data are
		// loaded, so writing a new production limit will succeed.
		if limit, err := h.uclpp.ProductionLimit(entity); err == nil {
			h.logger.Info("inverter reports production limit", "valueW", limit.Value, "active", limit.IsActive)
		}

		h.mu.Lock()
		already := h.wrote
		h.mu.Unlock()
		if already {
			return
		}
		h.logger.Info("sending production limit to inverter")
		h.writeLimit(entity)
	}
}

func (h *energyGuard) writeLimit(entity spineapi.EntityRemoteInterface) {
	limit := ucapi.LoadLimit{Duration: 2 * time.Minute, IsActive: true, Value: h.limitW}

	resultCB := func(msg model.ResultDataType) {
		if msg.ErrorNumber != nil && *msg.ErrorNumber == model.ErrorNumberTypeNoError {
			h.logger.Info("production limit accepted by inverter", "valueW", h.limitW)
			return
		}
		description := ""
		if msg.Description != nil {
			description = string(*msg.Description)
		}
		h.logger.Warn("production limit rejected", "code", msg.ErrorNumber, "description", description)
	}

	msgCounter, err := h.uclpp.WriteProductionLimit(entity, limit, resultCB)
	if err != nil {
		h.logger.Error("failed to write production limit", "error", err)
		return
	}

	h.mu.Lock()
	h.wrote = true
	h.mu.Unlock()
	h.logger.Info("production limit sent", "valueW", h.limitW, "msgCounter", msgCounter)
}

// ServiceReaderInterface

func (h *energyGuard) RemoteSKIConnected(_ eebusapi.ServiceInterface, ski string) {
	h.logger.Info("connected to inverter", "ski", ski)
}

func (h *energyGuard) RemoteSKIDisconnected(_ eebusapi.ServiceInterface, ski string) {
	h.logger.Info("disconnected from inverter", "ski", ski)
}

func (h *energyGuard) VisibleRemoteServicesUpdated(_ eebusapi.ServiceInterface, _ []shipapi.RemoteService) {
}

func (h *energyGuard) ServiceShipIDUpdate(_ string, _ string) {}

func (h *energyGuard) ServicePairingDetailUpdate(_ string, _ *shipapi.ConnectionStateDetail) {}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}
