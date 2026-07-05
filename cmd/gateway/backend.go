package main

import "time"

// backend is one controllable device the gateway can route commands to,
// regardless of the field protocol it speaks (EEBUS, Modbus, ...). The HTTP
// layer never needs to know which protocol a target uses.
type backend interface {
	SetLimit(watts float64, duration time.Duration) error
	Reset() error
	Status() backendStatus
}

// limitRequest is the JSON body of POST /limit. Target selects which device the
// command is routed to (defaults to "inverter").
type limitRequest struct {
	Target          string  `json:"target"`
	Watts           float64 `json:"watts"`
	DurationSeconds int     `json:"durationSeconds"`
	Reset           bool    `json:"reset"`
}

// backendStatus is the per-device status returned (keyed by target) by
// GET /status.
type backendStatus struct {
	Protocol         string   `json:"protocol"`
	Connected        bool     `json:"connected"`
	LastLimitW       *float64 `json:"lastLimitW"`
	MomentaryPowerW  *float64 `json:"momentaryPowerW,omitempty"`
	EnergyProducedWh *float64 `json:"energyProducedWh,omitempty"`
	VoltageV         *float64 `json:"voltageV,omitempty"`
	FrequencyHz      *float64 `json:"frequencyHz,omitempty"`
}
