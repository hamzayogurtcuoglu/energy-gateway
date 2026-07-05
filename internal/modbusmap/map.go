// Package modbusmap defines the Modbus register layout shared by the Modbus
// inverter simulator (server side) and the gateway (client side) so both agree
// on which register means what.
package modbusmap

// Holding registers (read with 0x03, written with 0x06/0x10) carry the
// controllable production limit.
const (
	HoldingLimitActive uint16 = 0 // 1 = production limit active, 0 = no limit
	HoldingLimitWatts  uint16 = 1 // production limit value in watts
	HoldingCount       uint16 = 2 // number of holding registers
)

// Input registers (read with 0x04) expose the live measurements.
const (
	InputActivePowerW uint16 = 0 // int16, watts (negative = production)
	InputPVPowerW     uint16 = 1 // uint16, watts
	InputFreqCentiHz  uint16 = 2 // uint16, hertz * 100
	InputSoCPercent   uint16 = 3 // uint16, percent (0..100)
	InputCount        uint16 = 4 // number of input registers
)
