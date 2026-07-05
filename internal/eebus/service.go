package eebus

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	eebusapi "github.com/enbility/eebus-go/api"
	featureserver "github.com/enbility/eebus-go/features/server"
	eebusservice "github.com/enbility/eebus-go/service"
	ucapi "github.com/enbility/eebus-go/usecases/api"
	"github.com/enbility/eebus-go/usecases/cs/lpp"
	shipapi "github.com/enbility/ship-go/api"
	"github.com/enbility/ship-go/cert"
	spineapi "github.com/enbility/spine-go/api"
	"github.com/enbility/spine-go/model"
	"github.com/enbility/spine-go/util"

	"energy-gateway/internal/inverter"
)

type Config struct {
	Port           int
	RemoteSKI      string
	AutoAccept     bool
	Interfaces     []string
	CertificatePEM string
	PrivateKeyPEM  string
	VendorCode     string
	Brand          string
	Model          string
	SerialNumber   string
	NominalPeakW   float64
}

type Service struct {
	simulator   *inverter.Simulator
	logger      *slog.Logger
	service     *eebusservice.Service
	production  *lpp.LPP
	measurement *featureserver.Measurement
	ids         measurementIDs
}

type measurementIDs struct {
	activePower    model.MeasurementIdType
	energyProduced model.MeasurementIdType
	soc            model.MeasurementIdType
	voltage        model.MeasurementIdType
	frequency      model.MeasurementIdType
}

func Start(ctx context.Context, simulator *inverter.Simulator, config Config, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	config = withDefaults(config)

	certificate, ski, err := loadOrCreateCertificate(config)
	if err != nil {
		return nil, err
	}

	configuration, err := eebusapi.NewConfiguration(
		config.VendorCode,
		config.Brand,
		config.Model,
		config.SerialNumber,
		model.DeviceTypeTypeInverter,
		[]model.EntityTypeType{model.EntityTypeTypeInverter},
		config.Port,
		certificate,
		4*time.Second,
	)
	if err != nil {
		return nil, err
	}
	configuration.SetAlternateIdentifier(fmt.Sprintf("%s-%s-%s", config.Brand, config.Model, config.SerialNumber))
	if len(config.Interfaces) > 0 {
		configuration.SetInterfaces(config.Interfaces)
	}

	realService := &Service{simulator: simulator, logger: logger}
	realService.service = eebusservice.NewService(configuration, realService)
	realService.service.SetLogging((*slogAdapter)(logger))

	if err := realService.service.Setup(); err != nil {
		return nil, err
	}

	localEntity := realService.service.LocalDevice().EntityForType(model.EntityTypeTypeInverter)
	if localEntity == nil {
		return nil, fmt.Errorf("local inverter entity was not created")
	}

	realService.production = lpp.NewLPP(localEntity, realService.handleProductionEvent)
	realService.service.AddUseCase(realService.production)
	if err := realService.production.SetProductionNominalMax(config.NominalPeakW); err != nil {
		logger.Warn("failed to set EEBUS production nominal max", "error", err)
	}
	if err := realService.production.SetProductionLimit(ucapi.LoadLimit{Value: config.NominalPeakW, IsChangeable: true, IsActive: false}); err != nil {
		logger.Warn("failed to set EEBUS production limit", "error", err)
	}
	if err := realService.production.SetFailsafeProductionActivePowerLimit(config.NominalPeakW, true); err != nil {
		logger.Warn("failed to set EEBUS failsafe production limit", "error", err)
	}
	if err := realService.production.SetFailsafeDurationMinimum(2*time.Hour, true); err != nil {
		logger.Warn("failed to set EEBUS failsafe duration", "error", err)
	}

	if err := realService.setupMeasurements(localEntity); err != nil {
		return nil, err
	}

	realService.service.SetAutoAccept(config.AutoAccept)
	realService.service.UserIsAbleToApproveOrCancelPairingRequests(true)
	if config.RemoteSKI != "" {
		realService.service.RegisterRemoteSKI(config.RemoteSKI)
	}

	realService.service.Start()
	go realService.publishMeasurements(ctx)

	logger.Info("real EEBUS inverter service listening", "port", config.Port, "ski", ski, "autoAccept", config.AutoAccept)
	return realService, nil
}

func (s *Service) Shutdown() {
	if s != nil && s.service != nil {
		s.service.Shutdown()
	}
}

func (s *Service) setupMeasurements(localEntity spineapi.EntityLocalInterface) error {
	measurementFeature := localEntity.GetOrAddFeature(model.FeatureTypeTypeMeasurement, model.RoleTypeServer)
	measurementFeature.AddFunctionType(model.FunctionTypeMeasurementDescriptionListData, true, false)
	measurementFeature.AddFunctionType(model.FunctionTypeMeasurementListData, true, false)

	electricalFeature := localEntity.GetOrAddFeature(model.FeatureTypeTypeElectricalConnection, model.RoleTypeServer)
	electricalFeature.AddFunctionType(model.FunctionTypeElectricalConnectionDescriptionListData, true, false)
	electricalFeature.AddFunctionType(model.FunctionTypeElectricalConnectionParameterDescriptionListData, true, false)
	electricalFeature.AddFunctionType(model.FunctionTypeElectricalConnectionCharacteristicListData, true, false)

	measurement, err := featureserver.NewMeasurement(localEntity)
	if err != nil {
		return err
	}
	electricalConnection, err := featureserver.NewElectricalConnection(localEntity)
	if err != nil {
		return err
	}

	connectionID := model.ElectricalConnectionIdType(0)
	phaseCount := uint(3)
	if err := electricalConnection.AddDescription(model.ElectricalConnectionDescriptionDataType{
		ElectricalConnectionId:  util.Ptr(connectionID),
		PowerSupplyType:         util.Ptr(model.ElectricalConnectionVoltageTypeTypeAc),
		AcConnectedPhases:       util.Ptr(phaseCount),
		PositiveEnergyDirection: util.Ptr(model.EnergyDirectionTypeConsume),
		ScopeType:               util.Ptr(model.ScopeTypeTypeACPowerTotal),
		Label:                   util.Ptr(model.LabelType("grid connection")),
	}); err != nil {
		return err
	}

	ids := measurementIDs{}
	ids.activePower = mustMeasurementID(measurement.AddDescription(measurementDescription(
		model.MeasurementTypeTypePower,
		model.UnitOfMeasurementTypeW,
		model.ScopeTypeTypeACPowerTotal,
		"active power total",
	)))
	ids.energyProduced = mustMeasurementID(measurement.AddDescription(measurementDescription(
		model.MeasurementTypeTypeEnergy,
		model.UnitOfMeasurementTypeWh,
		model.ScopeTypeTypeACEnergyProduced,
		"energy produced",
	)))
	ids.soc = mustMeasurementID(measurement.AddDescription(measurementDescription(
		model.MeasurementTypeTypePercentage,
		model.UnitOfMeasurementTypepct,
		model.ScopeTypeTypeStateOfCharge,
		"battery state of charge",
	)))
	ids.voltage = mustMeasurementID(measurement.AddDescription(measurementDescription(
		model.MeasurementTypeTypeVoltage,
		model.UnitOfMeasurementTypeV,
		model.ScopeTypeTypeACVoltage,
		"ac voltage",
	)))
	ids.frequency = mustMeasurementID(measurement.AddDescription(measurementDescription(
		model.MeasurementTypeTypeFrequency,
		model.UnitOfMeasurementTypeHz,
		model.ScopeTypeTypeACFrequency,
		"ac frequency",
	)))

	for _, item := range []struct {
		id    model.MeasurementIdType
		scope model.ScopeTypeType
		label string
	}{
		{ids.activePower, model.ScopeTypeTypeACPowerTotal, "active power total"},
		{ids.energyProduced, model.ScopeTypeTypeACEnergyProduced, "energy produced"},
		{ids.soc, model.ScopeTypeTypeStateOfCharge, "battery state of charge"},
		{ids.voltage, model.ScopeTypeTypeACVoltage, "ac voltage"},
		{ids.frequency, model.ScopeTypeTypeACFrequency, "ac frequency"},
	} {
		parameterDescription := model.ElectricalConnectionParameterDescriptionDataType{
			ElectricalConnectionId:  util.Ptr(connectionID),
			MeasurementId:           util.Ptr(item.id),
			VoltageType:             util.Ptr(model.ElectricalConnectionVoltageTypeTypeAc),
			AcMeasuredPhases:        util.Ptr(model.ElectricalConnectionPhaseNameTypeAbc),
			AcMeasuredInReferenceTo: util.Ptr(model.ElectricalConnectionPhaseNameTypeNeutral),
			AcMeasurementType:       util.Ptr(model.ElectricalConnectionAcMeasurementTypeTypeReal),
			AcMeasurementVariant:    util.Ptr(model.ElectricalConnectionMeasurandVariantTypeRms),
			ScopeType:               util.Ptr(item.scope),
			Label:                   util.Ptr(model.LabelType(item.label)),
		}
		if electricalConnection.AddParameterDescription(parameterDescription) == nil {
			return fmt.Errorf("failed to add electrical parameter description for %s", item.label)
		}
	}

	s.measurement = measurement
	s.ids = ids
	s.publishState(simulatorSnapshot(s.simulator))
	return nil
}

func measurementDescription(measurementType model.MeasurementTypeType, unit model.UnitOfMeasurementType, scope model.ScopeTypeType, label string) model.MeasurementDescriptionDataType {
	return model.MeasurementDescriptionDataType{
		MeasurementType: util.Ptr(measurementType),
		CommodityType:   util.Ptr(model.CommodityTypeTypeElectricity),
		Unit:            util.Ptr(unit),
		ScopeType:       util.Ptr(scope),
		Label:           util.Ptr(model.LabelType(label)),
	}
}

func mustMeasurementID(id *model.MeasurementIdType) model.MeasurementIdType {
	if id == nil {
		return 0
	}
	return *id
}

func (s *Service) publishMeasurements(ctx context.Context) {
	states, cancel := s.simulator.Subscribe(8)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case state, ok := <-states:
			if !ok {
				return
			}
			s.publishState(state)
		}
	}
}

func (s *Service) publishState(state inverter.State) {
	if s.measurement == nil {
		return
	}

	activePower := -state.ActivePowerW
	values := map[model.MeasurementIdType]float64{
		s.ids.activePower:    activePower,
		s.ids.energyProduced: -state.EnergyProducedWh,
		s.ids.soc:            state.BatterySoCPercent,
		s.ids.voltage:        state.VoltageV,
		s.ids.frequency:      state.FrequencyHz,
	}
	for id, value := range values {
		if err := s.measurement.UpdateDataForId(measurementData(state.Timestamp, value), nil, id); err != nil {
			s.logger.Debug("failed to publish EEBUS measurement", "id", id, "error", err)
		}
	}
}

func measurementData(timestamp time.Time, value float64) model.MeasurementDataType {
	return model.MeasurementDataType{
		ValueType:   util.Ptr(model.MeasurementValueTypeTypeValue),
		Timestamp:   model.NewAbsoluteOrRelativeTimeTypeFromTime(timestamp),
		Value:       model.NewScaledNumberType(value),
		ValueSource: util.Ptr(model.MeasurementValueSourceTypeMeasuredValue),
		ValueState:  util.Ptr(model.MeasurementValueStateTypeNormal),
	}
}

func (s *Service) handleProductionEvent(ski string, _ spineapi.DeviceRemoteInterface, _ spineapi.EntityRemoteInterface, event eebusapi.EventType) {
	if event != lpp.WriteApprovalRequired {
		return
	}

	for msgCounter, limit := range s.production.PendingProductionLimits() {
		s.production.ApproveOrDenyProductionLimit(msgCounter, true, "")
		if limit.IsActive {
			value := limit.Value
			s.simulator.ApplyControl(inverter.Control{MaxActivePowerW: &value})
			s.logger.Info("accepted EEBUS production limit", "ski", ski, "valueW", value)
			continue
		}
		s.simulator.ApplyControl(inverter.Control{ResetLimits: true})
		s.logger.Info("cleared EEBUS production limit", "ski", ski)
	}
}

func (s *Service) RemoteSKIConnected(_ eebusapi.ServiceInterface, ski string) {
	s.logger.Info("EEBUS remote SKI connected", "ski", ski)
}

func (s *Service) RemoteSKIDisconnected(_ eebusapi.ServiceInterface, ski string) {
	s.logger.Info("EEBUS remote SKI disconnected", "ski", ski)
}

func (s *Service) VisibleRemoteServicesUpdated(_ eebusapi.ServiceInterface, entries []shipapi.RemoteService) {
	for _, entry := range entries {
		s.logger.Info("visible EEBUS service", "name", entry.Name, "ski", entry.Ski, "identifier", entry.Identifier)
	}
}

func (s *Service) ServiceShipIDUpdate(ski string, shipID string) {
	s.logger.Info("EEBUS remote SHIP ID update", "ski", ski, "shipID", shipID)
}

func (s *Service) ServicePairingDetailUpdate(ski string, detail *shipapi.ConnectionStateDetail) {
	if detail == nil {
		return
	}
	s.logger.Info("EEBUS pairing state", "ski", ski, "state", detail.State())
}

func withDefaults(config Config) Config {
	if config.Port == 0 {
		config.Port = 4711
	}
	if config.VendorCode == "" {
		config.VendorCode = "SimGrid"
	}
	if config.Brand == "" {
		config.Brand = "SimGrid"
	}
	if config.Model == "" {
		config.Model = "InverterSimulator"
	}
	if config.SerialNumber == "" {
		config.SerialNumber = "SIM-INV-0001"
	}
	if config.CertificatePEM == "" {
		config.CertificatePEM = filepath.Join(".eebus", "inverter.crt")
	}
	if config.PrivateKeyPEM == "" {
		config.PrivateKeyPEM = filepath.Join(".eebus", "inverter.key")
	}
	if config.NominalPeakW <= 0 {
		config.NominalPeakW = 10000
	}
	return config
}

func loadOrCreateCertificate(config Config) (tls.Certificate, string, error) {
	if fileExists(config.CertificatePEM) && fileExists(config.PrivateKeyPEM) {
		certificate, err := tls.LoadX509KeyPair(config.CertificatePEM, config.PrivateKeyPEM)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		ski, err := skiFromTLSCertificate(certificate)
		return certificate, ski, err
	}

	certificate, err := cert.CreateCertificate(config.Brand, config.Brand, "DE", fmt.Sprintf("%s-%s", config.Model, config.SerialNumber))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writeCertificate(config.CertificatePEM, config.PrivateKeyPEM, certificate); err != nil {
		return tls.Certificate{}, "", err
	}
	ski, err := skiFromTLSCertificate(certificate)
	return certificate, ski, err
}

func writeCertificate(certPath, keyPath string, certificate tls.Certificate) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	privateKey, ok := certificate.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("certificate private key is %T, expected *ecdsa.PrivateKey", certificate.PrivateKey)
	}
	privateKeyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKeyBytes})

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, keyPEM, 0o600)
}

func skiFromTLSCertificate(certificate tls.Certificate) (string, error) {
	if len(certificate.Certificate) == 0 {
		return "", fmt.Errorf("certificate contains no leaf certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return "", err
	}
	return cert.SkiFromCertificate(leaf)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func simulatorSnapshot(simulator *inverter.Simulator) inverter.State {
	if simulator == nil {
		return inverter.State{}
	}
	return simulator.Snapshot()
}

type slogAdapter slog.Logger

func (l *slogAdapter) Trace(args ...interface{}) { (*slog.Logger)(l).Debug(fmt.Sprint(args...)) }
func (l *slogAdapter) Tracef(format string, args ...interface{}) {
	(*slog.Logger)(l).Debug(fmt.Sprintf(format, args...))
}
func (l *slogAdapter) Debug(args ...interface{}) { (*slog.Logger)(l).Debug(fmt.Sprint(args...)) }
func (l *slogAdapter) Debugf(format string, args ...interface{}) {
	(*slog.Logger)(l).Debug(fmt.Sprintf(format, args...))
}
func (l *slogAdapter) Info(args ...interface{}) { (*slog.Logger)(l).Info(fmt.Sprint(args...)) }
func (l *slogAdapter) Infof(format string, args ...interface{}) {
	(*slog.Logger)(l).Info(fmt.Sprintf(format, args...))
}
func (l *slogAdapter) Error(args ...interface{}) { (*slog.Logger)(l).Error(fmt.Sprint(args...)) }
func (l *slogAdapter) Errorf(format string, args ...interface{}) {
	(*slog.Logger)(l).Error(fmt.Sprintf(format, args...))
}
