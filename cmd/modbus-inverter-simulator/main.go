// Command modbus-inverter-simulator runs the shared inverter physics model as a
// Modbus TCP device. A holding register carries the production limit; input
// registers expose the live measurements. It mirrors the EEBUS inverter but
// speaks Modbus, so the gateway can route commands to either one.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/simonvetter/modbus"

	"eebus-inverter-simulator/internal/inverter"
	"eebus-inverter-simulator/internal/modbusmap"
)

// handler bridges Modbus register access to the inverter simulator.
type handler struct {
	simulator *inverter.Simulator
	logger    *slog.Logger

	mu          sync.Mutex
	limitActive uint16
	limitWatts  uint16
}

func (h *handler) HandleCoils(*modbus.CoilsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}

func (h *handler) HandleDiscreteInputs(*modbus.DiscreteInputsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}

func (h *handler) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	if req.Addr+req.Quantity > modbusmap.HoldingCount {
		return nil, modbus.ErrIllegalDataAddress
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if req.IsWrite {
		for i := uint16(0); i < req.Quantity; i++ {
			switch req.Addr + i {
			case modbusmap.HoldingLimitActive:
				h.limitActive = req.Args[i]
			case modbusmap.HoldingLimitWatts:
				h.limitWatts = req.Args[i]
			}
		}
		h.applyLimitLocked()
	}

	res := make([]uint16, req.Quantity)
	for i := uint16(0); i < req.Quantity; i++ {
		switch req.Addr + i {
		case modbusmap.HoldingLimitActive:
			res[i] = h.limitActive
		case modbusmap.HoldingLimitWatts:
			res[i] = h.limitWatts
		}
	}
	return res, nil
}

func (h *handler) HandleInputRegisters(req *modbus.InputRegistersRequest) ([]uint16, error) {
	if req.Addr+req.Quantity > modbusmap.InputCount {
		return nil, modbus.ErrIllegalDataAddress
	}

	state := h.simulator.Snapshot()
	res := make([]uint16, req.Quantity)
	for i := uint16(0); i < req.Quantity; i++ {
		switch req.Addr + i {
		case modbusmap.InputActivePowerW:
			res[i] = uint16(int16(clampRound(state.ActivePowerW, -32768, 32767)))
		case modbusmap.InputPVPowerW:
			res[i] = uint16(clampRound(state.PVPowerW, 0, math.MaxUint16))
		case modbusmap.InputFreqCentiHz:
			res[i] = uint16(clampRound(state.FrequencyHz*100, 0, math.MaxUint16))
		case modbusmap.InputSoCPercent:
			res[i] = uint16(clampRound(state.BatterySoCPercent, 0, math.MaxUint16))
		}
	}
	return res, nil
}

// applyLimitLocked pushes the current limit registers into the simulator.
func (h *handler) applyLimitLocked() {
	if h.limitActive == 1 {
		watts := float64(h.limitWatts)
		h.simulator.ApplyControl(inverter.Control{MaxActivePowerW: &watts})
		h.logger.Info("accepted Modbus production limit", "valueW", h.limitWatts)
	} else {
		h.simulator.ApplyControl(inverter.Control{ResetLimits: true})
		h.logger.Info("cleared Modbus production limit")
	}
}

func clampRound(value, low, high float64) int {
	if value < low {
		value = low
	}
	if value > high {
		value = high
	}
	return int(math.Round(value))
}

func main() {
	modbusURL := flag.String("modbus-url", "tcp://0.0.0.0:5502", "Modbus TCP listen URL")
	tick := flag.Duration("tick", time.Second, "simulation tick interval")
	nominalPeak := flag.Float64("nominal-peak-w", 10000, "nominal PV peak power in watts")
	batteryCapacity := flag.Float64("battery-capacity-wh", 12000, "battery capacity in watt-hours")
	initialSoC := flag.Float64("initial-soc", 55, "initial battery state of charge in percent")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	simulator := inverter.NewSimulator(inverter.Config{
		NominalPeakW:      *nominalPeak,
		BatteryCapacityWh: *batteryCapacity,
		InitialSoCPercent: *initialSoC,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go simulator.Run(ctx, *tick, time.Now)

	server, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL:        *modbusURL,
		Timeout:    5 * time.Minute,
		MaxClients: 5,
	}, &handler{simulator: simulator, logger: logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create modbus server: %v\n", err)
		os.Exit(1)
	}
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start modbus server: %v\n", err)
		os.Exit(1)
	}
	defer server.Stop()

	go logStatus(ctx, simulator, logger)

	logger.Info("Modbus inverter simulator running", "url", *modbusURL)
	<-ctx.Done()
	logger.Info("Modbus inverter simulator shutting down")
}

func logStatus(ctx context.Context, simulator *inverter.Simulator, logger *slog.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			state := simulator.Snapshot()
			limit := "none"
			if state.MaxActivePowerW != nil {
				limit = fmt.Sprintf("%.0fW", *state.MaxActivePowerW)
			}
			logger.Info("modbus inverter status",
				"mode", state.OperationMode,
				"pvW", state.PVPowerW,
				"activeW", state.ActivePowerW,
				"socPct", state.BatterySoCPercent,
				"limit", limit)
		}
	}
}
