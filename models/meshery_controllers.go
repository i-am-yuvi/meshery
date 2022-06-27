package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/layer5io/meshkit/broker/nats"
	"github.com/layer5io/meshkit/database"
	"github.com/layer5io/meshkit/logger"
	"github.com/layer5io/meshkit/models/controllers"
	"github.com/layer5io/meshkit/utils"
	mesherykube "github.com/layer5io/meshkit/utils/kubernetes"
	"github.com/spf13/viper"
)

const (
	chartRepo                     = "https://meshery.github.io/meshery.io/charts"
	MesheryServerBrokerConnection = "meshery-server"
)

type MesheryController int

const (
	MesheryBroker MesheryController = iota
	Meshsync
	MesheryOperator
)

type MesheryControllersHelper struct {
	//  maps each context with the controller handlers
	ctxControllerHandlersMap map[string]map[MesheryController]controllers.IMesheryController
	// maps each context with it's operator status
	ctxOperatorStatusMap map[string]controllers.MesheryControllerStatus
	// maps each context with a meshsync data handler
	ctxMeshsyncDataHandlerMap map[string]MeshsyncDataHandler
	log                       logger.Handler
	oprDepConfig              controllers.OperatorDeploymentConfig
	dbHandler                 *database.Handler
}

func (mch *MesheryControllersHelper) GetControllerHandlersForEachContext() map[string]map[MesheryController]controllers.IMesheryController {
	return mch.ctxControllerHandlersMap
}

func (mch *MesheryControllersHelper) GetOperatorsStatusMap() map[string]controllers.MesheryControllerStatus {
	return mch.ctxOperatorStatusMap
}

func NewMesheryControllersHelper(log logger.Handler, operatorDepConfig controllers.OperatorDeploymentConfig, dbHandler *database.Handler) *MesheryControllersHelper {
	return &MesheryControllersHelper{
		ctxControllerHandlersMap:  make(map[string]map[MesheryController]controllers.IMesheryController),
		log:                       log,
		oprDepConfig:              operatorDepConfig,
		ctxOperatorStatusMap:      make(map[string]controllers.MesheryControllerStatus),
		ctxMeshsyncDataHandlerMap: make(map[string]MeshsyncDataHandler),
		dbHandler:                 dbHandler,
	}
}

// initializes Meshsync data handler for the contexts for whom it has not been
// initialized yet. Apart from updating the map, it also runs the handler after
// updating the map. The presence of a handler for a context in a map indicate that
// the meshsync data for that context is properly being handled
func (mch *MesheryControllersHelper) UpdateMeshsynDataHandlers() *MesheryControllersHelper {
	// only checking those contexts whose MesheryConrollers are active
	for ctxId, controllerHandlers := range mch.ctxControllerHandlersMap {
		if _, ok := mch.ctxMeshsyncDataHandlerMap[ctxId]; !ok {
			brokerEndpoint, err := controllerHandlers[MesheryBroker].GetPublicEndpoint()
			if brokerEndpoint == "" {
				if err != nil {
					mch.log.Warn(err)
				}
				mch.log.Info(fmt.Sprintf("skipping meshsync data handler setup for contextId: %v as its public endpoint could not be found", ctxId))
				continue
			}
			mch.log.Info(fmt.Sprintf("found meshery-broker endpoint: %s for contextId: %s", brokerEndpoint, ctxId))
			brokerHandler, err := nats.New(nats.Options{
				// URLS: []string{"localhost:4222"},
				URLS:           []string{brokerEndpoint},
				ConnectionName: MesheryServerBrokerConnection,
				Username:       "",
				Password:       "",
				ReconnectWait:  2 * time.Second,
				MaxReconnect:   60,
			})
			if err != nil {
				mch.log.Warn(err)
				mch.log.Info(fmt.Sprintf("skipping meshsync data handler setup for contextId: %v due to: %v", ctxId, err.Error()))
				continue
			}
			mch.log.Info(fmt.Sprintf("broker connection sucessfully established for contextId: %v with meshery-broker at: %v", ctxId, brokerEndpoint))
			msDataHandler := NewMeshsyncDataHandler(brokerHandler, *mch.dbHandler, mch.log)
			err = msDataHandler.Run()
			if err != nil {
				mch.log.Warn(err)
				mch.log.Info(fmt.Sprintf("skipping meshsync data handler setup for contextId: %s due to: %s", ctxId, err.Error()))
				continue
			}
			mch.ctxMeshsyncDataHandlerMap[ctxId] = *msDataHandler
			mch.log.Info(fmt.Sprintf("meshsync data handler successfully setup for contextId: %s", ctxId))
		}
	}
	return mch
}

// attach a MesheryController for each context if
// 1. the config is valid
// 2. if it is not already attached
func (mch *MesheryControllersHelper) UpdateCtxControllerHandlers(ctxs []K8sContext) *MesheryControllersHelper {
	for _, ctx := range ctxs {
		ctxId := ctx.ID
		if _, ok := mch.ctxControllerHandlersMap[ctxId]; !ok {
			cfg, err := ctx.GenerateKubeConfig()
			client, err := mesherykube.New(cfg)
			// means that the config is invalid
			if err != nil {
				// invalid configs are not added to the map
				continue
			}
			mch.ctxControllerHandlersMap[ctxId] = map[MesheryController]controllers.IMesheryController{
				MesheryBroker:   controllers.NewMesheryBrokerHandler(client),
				MesheryOperator: controllers.NewMesheryOperatorHandler(client, mch.oprDepConfig),
				Meshsync:        controllers.NewMeshsyncHandler(client),
			}
		}
	}
	return mch
}

// update the status of MesheryOperator in all the contexts
// for whom MesheryControllers are attached
// should be called after UpdateCtxControllerHandlers
func (mch *MesheryControllersHelper) UpdateOperatorsStatusMap() *MesheryControllersHelper {
	for ctxId, ctrlHandler := range mch.ctxControllerHandlersMap {
		mch.ctxOperatorStatusMap[ctxId] = ctrlHandler[MesheryOperator].GetStatus()
	}
	return mch
}

// looks at the status of Meshery Operator for each cluster and takes necessary action.
// it will deploy the operator only when it is in NotDeployed state
func (mch *MesheryControllersHelper) DeployUndeployedOperators() *MesheryControllersHelper {
	for ctxId, ctrlHandler := range mch.ctxControllerHandlersMap {
		if oprStatus, ok := mch.ctxOperatorStatusMap[ctxId]; ok {
			if oprStatus == controllers.NotDeployed {
				err := ctrlHandler[MesheryOperator].Deploy()
				if err != nil {
					mch.log.Error(err)
				}
			}
		}
	}
	return mch
}

func NewOperatorDeploymentConfig(adapterTracker AdaptersTrackerInterface) controllers.OperatorDeploymentConfig {
	// get meshery release version
	mesheryReleaseVersion := viper.GetString("BUILD")
	if mesheryReleaseVersion == "" || mesheryReleaseVersion == "Not Set" || mesheryReleaseVersion == "edge-latest" {
		_, latestRelease, err := checkLatestVersion(mesheryReleaseVersion)
		// if unable to fetch latest release tag, meshkit helm functions handle
		// this automatically fetch the latest one
		if err != nil {
			// mch.log.Error(fmt.Errorf("Couldn't check release tag: %s. Will use latest version", err))
			mesheryReleaseVersion = ""
		} else {
			mesheryReleaseVersion = latestRelease
		}
	}

	return controllers.OperatorDeploymentConfig{
		MesheryReleaseVersion: mesheryReleaseVersion,
		GetHelmOverrides: func(delete bool) map[string]interface{} {
			return setOverrideValues(delete, adapterTracker)
		},
		HelmChartRepo: chartRepo,
	}

}

// checkLatestVersion takes in the current server version compares it with the target
// and returns the (isOutdated, latestVersion, error)
func checkLatestVersion(serverVersion string) (*bool, string, error) {
	// Inform user of the latest release version
	versions, err := utils.GetLatestReleaseTagsSorted("meshery", "meshery")
	latestVersion := versions[len(versions)-1]
	isOutdated := false
	if err != nil {
		return nil, "", ErrCreateOperatorDeploymentConfig(err)
	}
	// Compare current running Meshery server version to the latest available Meshery release on GitHub.
	if latestVersion != serverVersion {
		isOutdated = true
		return &isOutdated, latestVersion, nil
	}

	return &isOutdated, latestVersion, nil
}

// setOverrideValues detects the currently insalled adapters and sets appropriate
// overrides so as to not uninstall them. It also sets override values for
// operator so that it can be enabled or disabled depending on the need
func setOverrideValues(delete bool, adapterTracker AdaptersTrackerInterface) map[string]interface{} {
	installedAdapters := make([]string, 0)
	adapters := adapterTracker.GetAdapters(context.TODO())

	for _, adapter := range adapters {
		if adapter.Name != "" {
			installedAdapters = append(installedAdapters, strings.Split(adapter.Location, ":")[0])
		}
	}

	overrideValues := map[string]interface{}{
		"fullnameOverride": "meshery-operator",
		"meshery": map[string]interface{}{
			"enabled": false,
		},
		"meshery-istio": map[string]interface{}{
			"enabled": false,
		},
		"meshery-cilium": map[string]interface{}{
			"enabled": false,
		},
		"meshery-linkerd": map[string]interface{}{
			"enabled": false,
		},
		"meshery-consul": map[string]interface{}{
			"enabled": false,
		},
		"meshery-kuma": map[string]interface{}{
			"enabled": false,
		},
		"meshery-osm": map[string]interface{}{
			"enabled": false,
		},
		"meshery-nsm": map[string]interface{}{
			"enabled": false,
		},
		"meshery-nginx-sm": map[string]interface{}{
			"enabled": false,
		},
		"meshery-traefik-mesh": map[string]interface{}{
			"enabled": false,
		},
		"meshery-cpx": map[string]interface{}{
			"enabled": false,
		},
		"meshery-app-mesh": map[string]interface{}{
			"enabled": false,
		},
		"meshery-operator": map[string]interface{}{
			"enabled": true,
		},
	}

	for _, adapter := range installedAdapters {
		if _, ok := overrideValues[adapter]; ok {
			overrideValues[adapter] = map[string]interface{}{
				"enabled": true,
			}
		}
	}

	if delete {
		overrideValues["meshery-operator"] = map[string]interface{}{
			"enabled": false,
		}
	}

	return overrideValues
}
