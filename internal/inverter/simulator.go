package inverter

import (
	"context"
	"math"
	"sync"
	"time"
)

// Config configures the inverter simulator.
type Config struct {
	NominalPeakW      float64
	BatteryCapacityWh float64
	InitialSoCPercent float64
}

// State is the current simulated inverter state.
type State struct {
	Timestamp         time.Time
	OperationMode     string
	NominalPeakW      float64
	PVPowerW          float64
	ActivePowerW      float64
	BatterySoCPercent float64
	BatteryCapacityWh float64
	EnergyProducedWh  float64
	VoltageV          float64
	FrequencyHz       float64
	MaxActivePowerW   *float64
}

// Control carries the manipulation the EEBUS layer can apply.
// The only supported manipulation is the active power production limit,
// which mirrors the EEBUS CS-LPP use case.
type Control struct {
	MaxActivePowerW *float64
	ResetLimits     bool
}

// Simulator models the physical behaviour of a PV inverter with an
// optional battery. It produces power over a daylight curve and can be
// curtailed through an active power production limit.
type Simulator struct {
	mu          sync.RWMutex
	state       State
	socWh       float64
	subscribers map[chan State]struct{}
}

func NewSimulator(config Config) *Simulator {
	if config.NominalPeakW <= 0 {
		config.NominalPeakW = 10000
	}
	if config.BatteryCapacityWh < 0 {
		config.BatteryCapacityWh = 0
	}

	initialSoC := clamp(config.InitialSoCPercent, 0, 100)

	simulator := &Simulator{
		state: State{
			Timestamp:         time.Now().UTC(),
			OperationMode:     "standby",
			NominalPeakW:      config.NominalPeakW,
			BatterySoCPercent: initialSoC,
			BatteryCapacityWh: config.BatteryCapacityWh,
			VoltageV:          230,
			FrequencyHz:       50,
		},
		socWh:       config.BatteryCapacityWh * initialSoC / 100,
		subscribers: make(map[chan State]struct{}),
	}
	simulator.publishLocked()
	return simulator
}

// Run advances the simulation on a fixed interval until the context is done.
func (m *Simulator) Run(ctx context.Context, interval time.Duration, now func() time.Time) {
	if interval <= 0 {
		interval = time.Second
	}
	if now == nil {
		now = time.Now
	}

	previous := now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := now()
			m.Step(current, current.Sub(previous))
			previous = current
		}
	}
}

// Snapshot returns the current state.
func (m *Simulator) Snapshot() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// ApplyControl applies an EEBUS production limit to the simulator.
func (m *Simulator) ApplyControl(control Control) State {
	m.mu.Lock()
	defer m.mu.Unlock()

	if control.ResetLimits {
		m.state.MaxActivePowerW = nil
	}
	if control.MaxActivePowerW != nil {
		limit := clamp(*control.MaxActivePowerW, 0, m.state.NominalPeakW)
		m.state.MaxActivePowerW = &limit
	}

	m.refreshDerivedLocked(m.state.Timestamp, 0)
	m.publishLocked()
	return m.state
}

// Step advances the simulation by delta at the given time.
func (m *Simulator) Step(at time.Time, delta time.Duration) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshDerivedLocked(at, delta)
	m.publishLocked()
	return m.state
}

// Subscribe returns a channel that receives every state update.
func (m *Simulator) Subscribe(buffer int) (<-chan State, func()) {
	if buffer < 1 {
		buffer = 1
	}
	channel := make(chan State, buffer)

	m.mu.Lock()
	m.subscribers[channel] = struct{}{}
	channel <- m.state
	m.mu.Unlock()

	cancel := func() {
		m.mu.Lock()
		if _, ok := m.subscribers[channel]; ok {
			delete(m.subscribers, channel)
			close(channel)
		}
		m.mu.Unlock()
	}
	return channel, cancel
}

func (m *Simulator) refreshDerivedLocked(at time.Time, delta time.Duration) {
	if at.IsZero() {
		at = time.Now()
	}
	if delta < 0 {
		delta = 0
	}
	m.state.Timestamp = at.UTC()

	availablePower := m.state.NominalPeakW * daylightCurve(at)
	limit := m.state.NominalPeakW
	if m.state.MaxActivePowerW != nil {
		limit = *m.state.MaxActivePowerW
	}

	pvPower := math.Min(availablePower, limit)
	batteryPower := m.constrainBatteryPower(m.autoBatteryPower(availablePower, pvPower), delta)

	activePower := pvPower + batteryPower
	m.state.PVPowerW = round(pvPower, 1)
	m.state.ActivePowerW = round(activePower, 1)
	m.state.VoltageV = round(230+2*math.Sin(float64(at.Second())/60*2*math.Pi), 1)
	m.state.FrequencyHz = round(50+0.02*math.Sin(float64(at.Minute())/60*2*math.Pi), 3)

	switch {
	case pvPower == 0 && batteryPower == 0:
		m.state.OperationMode = "standby"
	case m.state.MaxActivePowerW != nil && availablePower > limit:
		m.state.OperationMode = "curtailed"
	default:
		m.state.OperationMode = "normal"
	}

	hours := delta.Hours()
	if hours > 0 {
		m.state.EnergyProducedWh = round(m.state.EnergyProducedWh+pvPower*hours, 3)
	}
}

func (m *Simulator) autoBatteryPower(availablePower, pvPower float64) float64 {
	if m.state.BatteryCapacityWh == 0 {
		return 0
	}
	soc := m.state.BatterySoCPercent
	sparePV := math.Max(availablePower-pvPower, 0)
	if sparePV > 0 && soc < 98 {
		return -math.Min(sparePV, m.batteryLimitW())
	}
	if pvPower > m.state.NominalPeakW*0.65 && soc < 95 {
		return -math.Min(pvPower*0.12, m.batteryLimitW())
	}
	if pvPower < m.state.NominalPeakW*0.1 && soc > 20 {
		return math.Min(800, m.batteryLimitW())
	}
	return 0
}

func (m *Simulator) constrainBatteryPower(power float64, delta time.Duration) float64 {
	if m.state.BatteryCapacityWh == 0 {
		return 0
	}
	power = clamp(power, -m.batteryLimitW(), m.batteryLimitW())
	hours := delta.Hours()
	if hours <= 0 {
		return power
	}

	if power < 0 {
		availableCapacity := m.state.BatteryCapacityWh - m.socWh
		power = -math.Min(-power, availableCapacity/hours)
	} else if power > 0 {
		power = math.Min(power, m.socWh/hours)
	}
	m.socWh = clamp(m.socWh-power*hours, 0, m.state.BatteryCapacityWh)
	m.state.BatterySoCPercent = round(100*m.socWh/m.state.BatteryCapacityWh, 2)
	return power
}

func (m *Simulator) batteryLimitW() float64 {
	return math.Min(5000, math.Max(0, m.state.BatteryCapacityWh/2))
}

func (m *Simulator) publishLocked() {
	state := m.state
	for subscriber := range m.subscribers {
		select {
		case subscriber <- state:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- state:
			default:
			}
		}
	}
}

func daylightCurve(at time.Time) float64 {
	local := at.Local()
	hour := float64(local.Hour()) + float64(local.Minute())/60 + float64(local.Second())/3600
	if hour < 6 || hour > 20 {
		return 0
	}
	return math.Sin((hour - 6) / 14 * math.Pi)
}

func clamp(value, minValue, maxValue float64) float64 {
	return math.Min(math.Max(value, minValue), maxValue)
}

func round(value float64, precision int) float64 {
	scale := math.Pow10(precision)
	return math.Round(value*scale) / scale
}
