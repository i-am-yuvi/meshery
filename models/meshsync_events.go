package models

import (
	"github.com/layer5io/meshkit/broker"
	"github.com/layer5io/meshkit/database"
	"github.com/layer5io/meshkit/logger"
	"github.com/layer5io/meshkit/utils"
	meshsyncmodel "github.com/layer5io/meshsync/pkg/model"
	"gorm.io/gorm"
)

const (
	MeshsyncStoreUpdatesSubject = "meshery-server.meshsync.store"
)

// TODO: Create proper error codes for the functionalities this struct implements

type MeshsyncDataHandler struct {
	broker    broker.Handler
	dbHandler database.Handler
	log       logger.Handler
}

func NewMeshsyncDataHandler(broker broker.Handler, dbHandler database.Handler, log logger.Handler) *MeshsyncDataHandler {
	return &MeshsyncDataHandler{
		broker:    broker,
		dbHandler: dbHandler,
		log:       log,
	}
}

func (mh *MeshsyncDataHandler) Run() error {

	err := mh.removeStaleObjects()
	if err != nil {
		return err
	}
	go mh.subsribeToStoreUpdates()
	go mh.subscribeToMeshsyncEvents()
	err = mh.requestMeshsyncStore()
	if err != nil {
		return err
	}
	return nil
}

func (mh *MeshsyncDataHandler) subscribeToMeshsyncEvents() {
	eventsChan := make(chan *broker.Message)
	err := mh.broker.SubscribeWithChannel("meshery.meshsync.core", "", eventsChan)
	if err != nil {
		mh.log.Error(err)
		return
	}
	mh.log.Info("subscribing to meshsync events on NATS subject: meshery.meshsync.core  ")

	for event := range eventsChan {
		if event.EventType == broker.ErrorEvent {
			// TODO: Handle errors accordingly
			mh.log.Error(event.Object.(error))
			continue
		}

		err := mh.meshsyncEventsAccumulator(event)
		if err != nil {
			mh.log.Error(err)
			continue
		}

	}

}

func (mh *MeshsyncDataHandler) subsribeToStoreUpdates() {
	storeChan := make(chan *broker.Message)
	mh.log.Info("subscribing to store updates from meshsync on NATS subject: ", MeshsyncStoreUpdatesSubject)
	err := mh.broker.SubscribeWithChannel(MeshsyncStoreUpdatesSubject, "", storeChan)
	if err != nil {
		mh.log.Error(err)
		return
	}

	for storeUpdate := range storeChan {

		if storeUpdate.EventType == broker.ErrorEvent {
			mh.log.Error(storeUpdate.Object.(error))
			continue
		}

		objectsSlice := storeUpdate.Object.([]interface{})

		for _, object := range objectsSlice {
			objectJSON, _ := utils.Marshal(object)
			obj := meshsyncmodel.Object{}
			err := utils.Unmarshal(objectJSON, &obj)
			if err != nil {
				mh.log.Error(err)
				continue
			}

			err = mh.persistStoreUpdate(&obj)
			if err != nil {
				mh.log.Error(err)
				continue
			}
		}

	}
}

// derives the state of the cluster from the events and persists it in the database
func (mh *MeshsyncDataHandler) meshsyncEventsAccumulator(event *broker.Message) error {

	mh.dbHandler.Lock()
	defer mh.dbHandler.Unlock()

	objectJSON, _ := utils.Marshal(event.Object)
	obj := meshsyncmodel.Object{}
	err := utils.Unmarshal(objectJSON, &obj)

	if err != nil {
		return err
	}

	switch event.EventType {
	case broker.Add, broker.Update:
		result := mh.dbHandler.Create(&obj)
		if result.Error != nil {
			result = mh.dbHandler.Session(&gorm.Session{FullSaveAssociations: true}).Updates(&obj)
			if result.Error != nil {
				return result.Error
			}
			return nil
		}

	case broker.Delete:
		result := mh.dbHandler.Delete(&obj)
		if result.Error != nil {
			return result.Error
		}
	}

	mh.log.Info("Updated database in response to ", event.EventType, " event of object: ", obj.ObjectMeta.Name, " in namespace: ", obj.ObjectMeta.Namespace, " of kind: ", obj.Kind)

	return nil
}

func (mh *MeshsyncDataHandler) persistStoreUpdate(object *meshsyncmodel.Object) error {

	mh.dbHandler.Lock()
	defer mh.dbHandler.Unlock()

	result := mh.dbHandler.Create(object)
	if result.Error != nil {
		result = mh.dbHandler.Session(&gorm.Session{FullSaveAssociations: true}).Updates(object)
		if result.Error != nil {
			return result.Error
		}
		mh.log.Info("Updated object: ", object.ObjectMeta.Name, "/", object.ObjectMeta.Namespace, " of kind: ", object.Kind, " in the database")
		return nil
	}
	mh.log.Info("Added object: ", object.ObjectMeta.Name, "/", object.ObjectMeta.Namespace, " of kind: ", object.Kind, " to the database")

	return nil
}

func (mh *MeshsyncDataHandler) removeStaleObjects() error {
	mh.dbHandler.Lock()
	defer mh.dbHandler.Unlock()

	mh.log.Info("Removing stale meshsync objects from the database")

	// Clear stale meshsync data
	err := mh.dbHandler.Migrator().DropTable(
		&meshsyncmodel.KeyValue{},
		&meshsyncmodel.Object{},
		&meshsyncmodel.ResourceSpec{},
		&meshsyncmodel.ResourceStatus{},
		&meshsyncmodel.ResourceObjectMeta{},
	)
	if err != nil {
		return err.(error)
	}
	err = mh.dbHandler.Migrator().CreateTable(
		&meshsyncmodel.KeyValue{},
		&meshsyncmodel.Object{},
		&meshsyncmodel.ResourceSpec{},
		&meshsyncmodel.ResourceStatus{},
		&meshsyncmodel.ResourceObjectMeta{},
	)
	if err != nil {
		return err.(error)
	}

	return nil
}

func (mh *MeshsyncDataHandler) requestMeshsyncStore() error {
	err := mh.broker.Publish("meshery.meshsync.request", &broker.Message{
		Request: &broker.RequestObject{
			Entity: "informer-store",
			// TODO: Name of the Reply subject should be taken from some sort of configuration
			Payload: struct{ Reply string }{Reply: "meshery-server.meshsync.store"},
		}})
	if err != nil {
		return err
	}
	return nil
}
