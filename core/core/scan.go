package core

import (
	"context"
	"fmt"

	"github.com/kubescape/go-logger"
	"github.com/kubescape/go-logger/helpers"
	"github.com/kubescape/k8s-interface/k8sinterface"
	"github.com/kubescape/k8s-interface/workloadinterface"
	"github.com/kubescape/kubescape/v2/core/cautils"
	"github.com/kubescape/kubescape/v2/core/cautils/getter"
	"github.com/kubescape/kubescape/v2/core/pkg/hostsensorutils"
	"github.com/kubescape/kubescape/v2/core/pkg/opaprocessor"
	"github.com/kubescape/kubescape/v2/core/pkg/policyhandler"
	"github.com/kubescape/kubescape/v2/core/pkg/resourcehandler"
	"github.com/kubescape/kubescape/v2/core/pkg/resourcesprioritization"
	"github.com/kubescape/kubescape/v2/core/pkg/resultshandling"
	"github.com/kubescape/kubescape/v2/core/pkg/resultshandling/printer"
	"github.com/kubescape/kubescape/v2/core/pkg/resultshandling/reporter"
	"github.com/kubescape/kubescape/v2/pkg/imagescan"
	apisv1 "github.com/kubescape/opa-utils/httpserver/apis/v1"
	"go.opentelemetry.io/otel"
	"golang.org/x/exp/slices"

	"github.com/kubescape/opa-utils/resources"
)

type componentInterfaces struct {
	tenantConfig      cautils.ITenantConfig
	resourceHandler   resourcehandler.IResourceHandler
	report            reporter.IReport
	uiPrinter         printer.IPrinter
	hostSensorHandler hostsensorutils.IHostSensor
	outputPrinters    []printer.IPrinter
}

func getInterfaces(ctx context.Context, scanInfo *cautils.ScanInfo) componentInterfaces {
	ctx, span := otel.Tracer("").Start(ctx, "setup interfaces")
	defer span.End()

	// ================== setup k8s interface object ======================================
	var k8s *k8sinterface.KubernetesApi
	if scanInfo.GetScanningContext() == cautils.ContextCluster {
		k8s = getKubernetesApi()
		if k8s == nil {
			logger.L().Ctx(ctx).Fatal("failed connecting to Kubernetes cluster")
		}
	}

	// ================== setup tenant object ======================================
	ctxTenant, spanTenant := otel.Tracer("").Start(ctx, "setup tenant")
	tenantConfig := getTenantConfig(&scanInfo.Credentials, k8sinterface.GetContextName(), scanInfo.CustomClusterName, k8s)

	// Set submit behavior AFTER loading tenant config
	setSubmitBehavior(scanInfo, tenantConfig)

	if scanInfo.Submit {
		// submit - Create tenant & Submit report
		if err := tenantConfig.SetTenant(); err != nil {
			logger.L().Ctx(ctxTenant).Error(err.Error())
		}

		if scanInfo.OmitRawResources {
			logger.L().Ctx(ctx).Warning("omit-raw-resources flag will be ignored in submit mode")
		}
	}
	spanTenant.End()

	// ================== version testing ======================================

	v := cautils.NewIVersionCheckHandler(ctx)
	v.CheckLatestVersion(ctx, cautils.NewVersionCheckRequest(cautils.BuildNumber, policyIdentifierIdentities(scanInfo.PolicyIdentifier), "", cautils.ScanningContextToScanningScope(scanInfo.GetScanningContext())))

	// ================== setup host scanner object ======================================
	ctxHostScanner, spanHostScanner := otel.Tracer("").Start(ctx, "setup host scanner")
	hostSensorHandler := getHostSensorHandler(ctx, scanInfo, k8s)
	if err := hostSensorHandler.Init(ctxHostScanner); err != nil {
		logger.L().Ctx(ctxHostScanner).Error("failed to init host scanner", helpers.Error(err))
		hostSensorHandler = hostsensorutils.NewHostSensorHandlerMock()
	}
	spanHostScanner.End()

	// ================== setup registry adaptors ======================================

	registryAdaptors, _ := resourcehandler.NewRegistryAdaptors()

	// ================== setup resource collector object ======================================

	resourceHandler := getResourceHandler(ctx, scanInfo, tenantConfig, k8s, hostSensorHandler, registryAdaptors)

	// ================== setup reporter & printer objects ======================================

	// reporting behavior - setup reporter
	reportHandler := getReporter(ctx, tenantConfig, scanInfo.ScanID, scanInfo.Submit, scanInfo.FrameworkScan, *scanInfo)

	// setup printers
	outputPrinters := GetOutputPrinters(scanInfo, ctx)

	uiPrinter := GetUIPrinter(ctx, scanInfo)

	// ================== return interface ======================================

	return componentInterfaces{
		tenantConfig:      tenantConfig,
		resourceHandler:   resourceHandler,
		report:            reportHandler,
		outputPrinters:    outputPrinters,
		uiPrinter:         uiPrinter,
		hostSensorHandler: hostSensorHandler,
	}
}

func GetOutputPrinters(scanInfo *cautils.ScanInfo, ctx context.Context) []printer.IPrinter {
	formats := scanInfo.Formats()

	outputPrinters := make([]printer.IPrinter, 0)
	for _, format := range formats {
		printerHandler := resultshandling.NewPrinter(ctx, format, scanInfo.FormatVersion, scanInfo.PrintAttackTree, scanInfo.VerboseMode, cautils.ViewTypes(scanInfo.View))
		printerHandler.SetWriter(ctx, scanInfo.Output)
		outputPrinters = append(outputPrinters, printerHandler)
	}
	return outputPrinters
}

func (ks *Kubescape) Scan(ctx context.Context, scanInfo *cautils.ScanInfo) (*resultshandling.ResultsHandler, error) {
	ctxInit, spanInit := otel.Tracer("").Start(ctx, "initialization")
	logger.L().Info("Kubescape scanner starting")

	// ===================== Initialization =====================
	scanInfo.Init(ctxInit) // initialize scan info

	interfaces := getInterfaces(ctxInit, scanInfo)

	cautils.ClusterName = interfaces.tenantConfig.GetContextName() // TODO - Deprecated
	cautils.CustomerGUID = interfaces.tenantConfig.GetAccountID()  // TODO - Deprecated
	interfaces.report.SetClusterName(interfaces.tenantConfig.GetContextName())
	interfaces.report.SetCustomerGUID(interfaces.tenantConfig.GetAccountID())

	downloadReleasedPolicy := getter.NewDownloadReleasedPolicy() // download config inputs from github release

	// set policy getter only after setting the customerGUID
	scanInfo.Getters.PolicyGetter = getPolicyGetter(ctxInit, scanInfo.UseFrom, interfaces.tenantConfig.GetTenantEmail(), scanInfo.FrameworkScan, downloadReleasedPolicy)
	scanInfo.Getters.ControlsInputsGetter = getConfigInputsGetter(ctxInit, scanInfo.ControlsInputs, interfaces.tenantConfig.GetAccountID(), downloadReleasedPolicy)
	scanInfo.Getters.ExceptionsGetter = getExceptionsGetter(ctxInit, scanInfo.UseExceptions, interfaces.tenantConfig.GetAccountID(), downloadReleasedPolicy)
	scanInfo.Getters.AttackTracksGetter = getAttackTracksGetter(ctxInit, scanInfo.AttackTracks, interfaces.tenantConfig.GetAccountID(), downloadReleasedPolicy)

	// TODO - list supported frameworks/controls
	if scanInfo.ScanAll {
		scanInfo.SetPolicyIdentifiers(listFrameworksNames(scanInfo.Getters.PolicyGetter), apisv1.KindFramework)
	}

	// remove host scanner components
	defer func() {
		if err := interfaces.hostSensorHandler.TearDown(); err != nil {
			logger.L().Ctx(ctx).Error("failed to tear down host scanner", helpers.Error(err))
		}
	}()

	resultsHandling := resultshandling.NewResultsHandler(interfaces.report, interfaces.outputPrinters, interfaces.uiPrinter)

	// ===================== policies =====================
	ctxPolicies, spanPolicies := otel.Tracer("").Start(ctxInit, "policies")
	policyHandler := policyhandler.NewPolicyHandler()
	scanData, err := policyHandler.CollectPolicies(ctxPolicies, scanInfo.PolicyIdentifier, scanInfo)
	if err != nil {
		spanInit.End()
		return resultsHandling, err
	}
	spanPolicies.End()

	// ===================== resources =====================
	ctxResources, spanResources := otel.Tracer("").Start(ctxInit, "resources")
	err = resourcehandler.CollectResources(ctxResources, interfaces.resourceHandler, scanInfo.PolicyIdentifier, scanData, cautils.NewProgressHandler(""), scanInfo)
	if err != nil {
		spanInit.End()
		return resultsHandling, err
	}
	spanResources.End()
	spanInit.End()

	// ========================= opa testing =====================
	ctxOpa, spanOpa := otel.Tracer("").Start(ctx, "opa testing")
	defer spanOpa.End()

	deps := resources.NewRegoDependenciesData(k8sinterface.GetK8sConfig(), interfaces.tenantConfig.GetContextName())
	reportResults := opaprocessor.NewOPAProcessor(scanData, deps)
	if err := reportResults.ProcessRulesListener(ctxOpa, cautils.NewProgressHandler("")); err != nil {
		// TODO - do something
		return resultsHandling, fmt.Errorf("%w", err)
	}

	// ======================== prioritization ===================
	if scanInfo.PrintAttackTree || isPrioritizationScanType(scanInfo.ScanType) {
		_, spanPrioritization := otel.Tracer("").Start(ctxOpa, "prioritization")
		if priotizationHandler, err := resourcesprioritization.NewResourcesPrioritizationHandler(ctxOpa, scanInfo.Getters.AttackTracksGetter, scanInfo.PrintAttackTree); err != nil {
			logger.L().Ctx(ctx).Warning("failed to get attack tracks, this may affect the scanning results", helpers.Error(err))
		} else if err := priotizationHandler.PrioritizeResources(scanData); err != nil {
			return resultsHandling, fmt.Errorf("%w", err)
		}
		if err == nil && isPrioritizationScanType(scanInfo.ScanType) {
			scanData.SetTopWorkloads()
		}
		spanPrioritization.End()
	}

	if scanInfo.ScanImages {
		scanImages(scanInfo.ScanType, scanData, ctx, resultsHandling)
	}
	// ========================= results handling =====================
	resultsHandling.SetData(scanData)

	// if resultsHandling.GetRiskScore() > float32(scanInfo.FailThreshold) {
	// 	return resultsHandling, fmt.Errorf("scan risk-score %.2f is above permitted threshold %.2f", resultsHandling.GetRiskScore(), scanInfo.FailThreshold)
	// }

	return resultsHandling, nil
}

func scanImages(scanType cautils.ScanTypes, scanData *cautils.OPASessionObj, ctx context.Context, resultsHandling *resultshandling.ResultsHandler) {
	imagesToScan := []string{}

	if scanType == cautils.ScanTypeWorkload {
		containers, err := workloadinterface.NewWorkloadObj(scanData.SingleResourceScan.GetObject()).GetContainers()
		if err != nil {
			logger.L().Error("failed to get containers", helpers.Error(err))
			return
		}
		for _, container := range containers {
			if !slices.Contains(imagesToScan, container.Image) {
				imagesToScan = append(imagesToScan, container.Image)
			}
		}
	} else {
		for _, workload := range scanData.AllResources {
			containers, err := workloadinterface.NewWorkloadObj(workload.GetObject()).GetContainers()
			if err != nil {
				logger.L().Error(fmt.Sprintf("failed to get containers for kind: %s, name: %s, namespace: %s", workload.GetKind(), workload.GetName(), workload.GetNamespace()), helpers.Error(err))
				continue
			}
			for _, container := range containers {
				if !slices.Contains(imagesToScan, container.Image) {
					imagesToScan = append(imagesToScan, container.Image)
				}
			}
		}
	}
	logger.L().Info("Scanning images")

	dbCfg, _ := imagescan.NewDefaultDBConfig()
	svc := imagescan.NewScanService(dbCfg)

	for _, img := range imagesToScan {
		scanSingleImage(ctx, img, svc, resultsHandling)
	}

	logger.L().Success("Finished scanning images")
}

func scanSingleImage(ctx context.Context, img string, svc imagescan.Service, resultsHandling *resultshandling.ResultsHandler) {
	logger.L().Ctx(ctx).Debug(fmt.Sprintf("Scanning image: %s", img))

	scanResults, err := svc.Scan(ctx, img, imagescan.RegistryCredentials{})
	if err != nil {
		logger.L().Ctx(ctx).Error(fmt.Sprintf("failed to scan image: %s", img), helpers.Error(err))
		return
	}

	resultsHandling.ImageScanData = append(resultsHandling.ImageScanData, cautils.ImageScanData{
		Image:           img,
		PresenterConfig: scanResults,
	})
}

func isPrioritizationScanType(scanType cautils.ScanTypes) bool {
	return scanType == cautils.ScanTypeCluster || scanType == cautils.ScanTypeRepo
}
