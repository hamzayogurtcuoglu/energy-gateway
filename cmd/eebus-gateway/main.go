// Command eebus-gateway bridges a local control API to EEBUS.
//
// It is an official-style EEBUS Energy Guard (enbility/eebus-go `eg/lpp`) that
// connects to an inverter implementing `cs/lpp`, and exposes a small localhost
// HTTP API so another process (for example a Matter node) can set or clear the
// inverter's active power production limit over real EEBUS SHIP/SPINE.
//
//	Matter controller --Matter--> Matter node --HTTP--> this gateway --EEBUS--> inverter
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
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
	"github.com/enbility/eebus-go/usecases/ma/mpc"
	shipapi "github.com/enbility/ship-go/api"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/enbility/spine-go/model"

	"eebus-inverter-simulator/internal/eebuscert"
)

type gateway struct {
	uclpp  ucapi.EgLPPInterface
	ucmpc  ucapi.MaMPCInterface
	logger *slog.Logger

	mu     sync.Mutex
	entity spineapi.EntityRemoteInterface
	lastW  *float64
}

type limitRequest struct {
	Watts           float64 `json:"watts"`
	DurationSeconds int     `json:"durationSeconds"`
	Reset           bool    `json:"reset"`
}

type statusResponse struct {
	Connected        bool     `json:"connected"`
	LastLimitW       *float64 `json:"lastLimitW"`
	MomentaryPowerW  *float64 `json:"momentaryPowerW,omitempty"`
	EnergyProducedWh *float64 `json:"energyProducedWh,omitempty"`
	VoltageV         *float64 `json:"voltageV,omitempty"`
	FrequencyHz      *float64 `json:"frequencyHz,omitempty"`
}

func main() {
	eebusPort := flag.Int("eebus-port", 47712, "EEBUS SHIP listen port")
	ifaces := flag.String("eebus-interface", "", "network interface names for mDNS, e.g. Wi-Fi")
	inverterSKI := flag.String("inverter-ski", "", "SKI of the inverter to control; empty just prints this gateway's SKI")
	httpAddr := flag.String("http", "127.0.0.1:8090", "local bridge API address the Matter node calls")
	certPath := flag.String("cert", ".gateway/gateway.crt", "TLS certificate path")
	keyPath := flag.String("key", ".gateway/gateway.key", "TLS private key path")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	certificate, ski, err := eebuscert.LoadOrCreate(*certPath, *keyPath, "EEBUS-Matter-Gateway")
	if err != nil {
		fmt.Fprintf(os.Stderr, "certificate error: %v\n", err)
		os.Exit(1)
	}

	configuration, err := eebusapi.NewConfiguration(
		"Demo", "Demo", "Gateway", "555555555",
		model.DeviceTypeTypeElectricitySupplySystem,
		[]model.EntityTypeType{model.EntityTypeTypeGridGuard},
		*eebusPort, certificate, 4*time.Second,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(1)
	}
	configuration.SetAlternateIdentifier("EEBUS-Matter-Gateway-555555555")
	if list := splitCSV(*ifaces); len(list) > 0 {
		configuration.SetInterfaces(list)
	}

	g := &gateway{logger: logger}
	svc := service.NewService(configuration, g)
	if err := svc.Setup(); err != nil {
		fmt.Fprintf(os.Stderr, "setup error: %v\n", err)
		os.Exit(1)
	}

	localEntity := svc.LocalDevice().EntityForType(model.EntityTypeTypeGridGuard)
	g.uclpp = lpp.NewLPP(localEntity, g.onLPPEvent)
	svc.AddUseCase(g.uclpp)
	g.ucmpc = mpc.NewMPC(localEntity, g.onMPCEvent)
	svc.AddUseCase(g.ucmpc)

	svc.SetAutoAccept(true)
	svc.Start()
	defer svc.Shutdown()

	if *inverterSKI != "" {
		svc.RegisterRemoteSKI(*inverterSKI)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /limit", g.handleLimit)
	mux.HandleFunc("GET /status", g.handleStatus)
	httpServer := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("EEBUS-Matter gateway running", "ski", ski, "eebusPort", *eebusPort, "http", *httpAddr, "inverterSKI", *inverterSKI)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "http server failed: %v\n", err)
		os.Exit(1)
	}
	logger.Info("gateway shutting down")
}

// onLPPEvent tracks the connected inverter entity so the HTTP handler can write
// production limits to it. The entity is usable once its LoadControl data has
// been loaded (DataUpdateLimit).
func (g *gateway) onLPPEvent(_ string, _ spineapi.DeviceRemoteInterface, entity spineapi.EntityRemoteInterface, event eebusapi.EventType) {
	switch event {
	case lpp.UseCaseSupportUpdate:
		g.logger.Info("inverter supports production limitation (CS-LPP)")
	case lpp.DataUpdateLimit:
		g.mu.Lock()
		g.entity = entity
		g.mu.Unlock()
		g.logger.Info("inverter ready to receive production limits")
	}
}

// onMPCEvent tracks the inverter entity and notes when live meter data (power,
// energy, voltage, frequency) becomes available over EEBUS via the Monitoring of
// Power Consumption use case.
func (g *gateway) onMPCEvent(_ string, _ spineapi.DeviceRemoteInterface, entity spineapi.EntityRemoteInterface, event eebusapi.EventType) {
	switch event {
	case mpc.UseCaseSupportUpdate:
		g.logger.Info("inverter supports power monitoring (MPC)")
	case mpc.DataUpdatePower, mpc.DataUpdateEnergyProduced, mpc.DataUpdateVoltagePerPhase, mpc.DataUpdateFrequency:
		g.mu.Lock()
		if g.entity == nil {
			g.entity = entity
		}
		g.mu.Unlock()
	}
}

func (g *gateway) handleLimit(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req limitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	g.mu.Lock()
	entity := g.entity
	g.mu.Unlock()
	if entity == nil {
		http.Error(w, "inverter not connected yet", http.StatusServiceUnavailable)
		return
	}

	var limit ucapi.LoadLimit
	if req.Reset {
		limit = ucapi.LoadLimit{IsActive: false, Value: 0}
	} else {
		duration := time.Duration(req.DurationSeconds) * time.Second
		if duration <= 0 {
			duration = 2 * time.Minute
		}
		limit = ucapi.LoadLimit{IsActive: true, Value: req.Watts, Duration: duration}
	}

	resultCh := make(chan model.ResultDataType, 1)
	resultCB := func(msg model.ResultDataType) {
		select {
		case resultCh <- msg:
		default:
		}
	}

	if _, err := g.uclpp.WriteProductionLimit(entity, limit, resultCB); err != nil {
		http.Error(w, fmt.Sprintf("failed to write production limit: %v", err), http.StatusBadGateway)
		return
	}

	select {
	case msg := <-resultCh:
		if msg.ErrorNumber == nil || *msg.ErrorNumber != model.ErrorNumberTypeNoError {
			description := ""
			if msg.Description != nil {
				description = string(*msg.Description)
			}
			http.Error(w, fmt.Sprintf("inverter rejected limit: %s", description), http.StatusBadGateway)
			return
		}
	case <-time.After(5 * time.Second):
		http.Error(w, "timed out waiting for inverter response", http.StatusGatewayTimeout)
		return
	}

	g.mu.Lock()
	if req.Reset {
		g.lastW = nil
	} else {
		value := req.Watts
		g.lastW = &value
	}
	g.mu.Unlock()

	g.logger.Info("applied production limit over EEBUS", "watts", req.Watts, "reset", req.Reset)
	g.writeStatus(w)
}

func (g *gateway) handleStatus(w http.ResponseWriter, _ *http.Request) {
	g.writeStatus(w)
}

func (g *gateway) writeStatus(w http.ResponseWriter) {
	g.mu.Lock()
	entity := g.entity
	resp := statusResponse{Connected: entity != nil, LastLimitW: g.lastW}
	g.mu.Unlock()

	if entity != nil {
		if v, err := g.ucmpc.Power(entity); err == nil {
			resp.MomentaryPowerW = &v
		}
		if v, err := g.ucmpc.EnergyProduced(entity); err == nil {
			resp.EnergyProducedWh = &v
		}
		if vs, err := g.ucmpc.VoltagePerPhase(entity); err == nil && len(vs) > 0 {
			v := vs[0]
			resp.VoltageV = &v
		}
		if v, err := g.ucmpc.Frequency(entity); err == nil {
			resp.FrequencyHz = &v
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ServiceReaderInterface

func (g *gateway) RemoteSKIConnected(_ eebusapi.ServiceInterface, ski string) {
	g.logger.Info("connected to inverter", "ski", ski)
}

func (g *gateway) RemoteSKIDisconnected(_ eebusapi.ServiceInterface, ski string) {
	g.mu.Lock()
	g.entity = nil
	g.mu.Unlock()
	g.logger.Info("disconnected from inverter", "ski", ski)
}

func (g *gateway) VisibleRemoteServicesUpdated(_ eebusapi.ServiceInterface, _ []shipapi.RemoteService) {
}

func (g *gateway) ServiceShipIDUpdate(_ string, _ string) {}

func (g *gateway) ServicePairingDetailUpdate(_ string, _ *shipapi.ConnectionStateDetail) {}

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
