package main

import (
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/simonvetter/modbus"

	"eebus-inverter-simulator/internal/modbusmap"
)

// modbusBackend controls a Modbus device: it writes the production limit into
// holding registers and reads live measurements from input registers.
type modbusBackend struct {
	logger *slog.Logger
	url    string

	mu     sync.Mutex
	client *modbus.ModbusClient
	opened bool
}

func newModbusBackend(logger *slog.Logger, url string) (*modbusBackend, error) {
	client, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL:     url,
		Timeout: 3 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("create modbus client: %w", err)
	}
	return &modbusBackend{logger: logger, url: url, client: client}, nil
}

func (b *modbusBackend) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropLocked()
}

// ensureOpenLocked lazily connects so the gateway can start before the Modbus
// device is up, and reconnect if the device restarts.
func (b *modbusBackend) ensureOpenLocked() error {
	if b.opened {
		return nil
	}
	if err := b.client.Open(); err != nil {
		return fmt.Errorf("modbus device unreachable at %s: %w", b.url, err)
	}
	b.opened = true
	return nil
}

func (b *modbusBackend) dropLocked() {
	if b.opened {
		_ = b.client.Close()
		b.opened = false
	}
}

// do runs a modbus operation, reconnecting and retrying once if the connection
// was dropped (for example by the device's idle timeout).
func (b *modbusBackend) do(op func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureOpenLocked(); err != nil {
		return err
	}
	if err := op(); err != nil {
		b.dropLocked()
		if err := b.ensureOpenLocked(); err != nil {
			return err
		}
		if err := op(); err != nil {
			b.dropLocked()
			return err
		}
	}
	return nil
}

func (b *modbusBackend) SetLimit(watts float64, _ time.Duration) error {
	if err := b.do(func() error {
		return b.client.WriteRegisters(modbusmap.HoldingLimitActive, []uint16{1, wattsToRegister(watts)})
	}); err != nil {
		return fmt.Errorf("modbus write limit: %w", err)
	}
	b.logger.Info("applied production limit over Modbus", "watts", watts)
	return nil
}

func (b *modbusBackend) Reset() error {
	if err := b.do(func() error {
		return b.client.WriteRegisters(modbusmap.HoldingLimitActive, []uint16{0, 0})
	}); err != nil {
		return fmt.Errorf("modbus clear limit: %w", err)
	}
	b.logger.Info("cleared production limit over Modbus")
	return nil
}

func (b *modbusBackend) Status() backendStatus {
	status := backendStatus{Protocol: "modbus"}

	var holding, input []uint16
	err := b.do(func() error {
		// Registers start at address 0, so each returned slice is indexed by
		// register address directly.
		h, err := b.client.ReadRegisters(modbusmap.HoldingLimitActive, modbusmap.HoldingCount, modbus.HOLDING_REGISTER)
		if err != nil {
			return err
		}
		i, err := b.client.ReadRegisters(modbusmap.InputActivePowerW, modbusmap.InputCount, modbus.INPUT_REGISTER)
		if err != nil {
			return err
		}
		holding, input = h, i
		return nil
	})
	if err != nil {
		return status
	}

	status.Connected = true
	if len(holding) >= 2 && holding[modbusmap.HoldingLimitActive] == 1 {
		watts := float64(holding[modbusmap.HoldingLimitWatts])
		status.LastLimitW = &watts
	}
	if len(input) >= 4 {
		power := float64(int16(input[modbusmap.InputActivePowerW]))
		status.MomentaryPowerW = &power
		frequency := float64(input[modbusmap.InputFreqCentiHz]) / 100
		status.FrequencyHz = &frequency
	}
	return status
}

func wattsToRegister(watts float64) uint16 {
	if watts < 0 {
		watts = 0
	}
	if watts > math.MaxUint16 {
		watts = math.MaxUint16
	}
	return uint16(math.Round(watts))
}
