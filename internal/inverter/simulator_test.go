package inverter

import (
	"testing"
	"time"
)

func TestStepProducesPowerDuringDaylight(t *testing.T) {
	simulator := NewSimulator(Config{NominalPeakW: 10000, BatteryCapacityWh: 10000, InitialSoCPercent: 50})
	state := simulator.Step(time.Date(2026, 7, 3, 12, 0, 0, 0, time.Local), time.Hour)

	if state.PVPowerW <= 0 {
		t.Fatalf("expected positive PV power, got %.1f", state.PVPowerW)
	}
	if state.EnergyProducedWh <= 0 {
		t.Fatalf("expected produced energy to increase, got %.3f", state.EnergyProducedWh)
	}
}

func TestEEBUSProductionLimitCurtailsPower(t *testing.T) {
	simulator := NewSimulator(Config{NominalPeakW: 10000, BatteryCapacityWh: 0})
	limit := 2500.0
	simulator.ApplyControl(Control{MaxActivePowerW: &limit})
	state := simulator.Step(time.Date(2026, 7, 3, 12, 0, 0, 0, time.Local), time.Minute)

	if state.PVPowerW > limit {
		t.Fatalf("expected PV power <= %.1f, got %.1f", limit, state.PVPowerW)
	}
	if state.OperationMode != "curtailed" {
		t.Fatalf("expected curtailed mode, got %q", state.OperationMode)
	}
}

func TestResetLimitsClearsProductionLimit(t *testing.T) {
	simulator := NewSimulator(Config{NominalPeakW: 10000, BatteryCapacityWh: 0})
	limit := 2500.0
	simulator.ApplyControl(Control{MaxActivePowerW: &limit})
	state := simulator.ApplyControl(Control{ResetLimits: true})

	if state.MaxActivePowerW != nil {
		t.Fatalf("expected no production limit after reset, got %.1f", *state.MaxActivePowerW)
	}
}
