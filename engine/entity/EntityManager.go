package entity

import (
	"reflect"

	"math/rand"

	"os"

	"strings"

	"github.com/pkg/errors"
	. "github.com/xiaonanln/goworld/engine/common"
	"github.com/xiaonanln/goworld/components/dispatcher/dispatcher_client"
	"github.com/xiaonanln/goworld/engine/consts"
	"github.com/xiaonanln/goworld/engine/gwlog"
	"github.com/xiaonanln/goworld/engine/gwutils"
	"github.com/xiaonanln/goworld/engine/post"
	"github.com/xiaonanln/goworld/engine/storage"
	"github.com/xiaonanln/typeconv"
)

var (
	registeredEntityTypes = map[string]*EntityTypeDesc{}
	entityManager         = newEntityManager()
)

type EntityTypeDesc struct {
	entityType      reflect.Type
	rpcDescs        RpcDescMap
	allClientAttrs  StringSet
	clientAttrs     StringSet
	persistentAttrs StringSet
}

var _VALID_ATTR_DEFS = StringSet{} // all valid attribute defs

func init() {
	_VALID_ATTR_DEFS.Add(strings.ToLower("Client"))
	_VALID_ATTR_DEFS.Add(strings.ToLower("AllClients"))
	_VALID_ATTR_DEFS.Add(strings.ToLower("Persistent"))
}

func (desc *EntityTypeDesc) DefineAttrs(attrDefs map[string][]string) {

	for attr, defs := range attrDefs {
		isAllClient, isClient, isPersistent := false, false, false

		for _, def := range defs {
			def := strings.ToLower(def)

			if !_VALID_ATTR_DEFS.Contains(def) {
				// not a valid def
				gwlog.Panicf("attribute %s: invalid property: %s; all valid properties: %v", attr, def, _VALID_ATTR_DEFS.ToList())
			}

			if def == "allclients" {
				isAllClient = true
				isClient = true
			} else if def == "client" {
				isClient = true
			} else if def == "persistent" {
				isPersistent = true
			}
		}

		if isAllClient {
			desc.allClientAttrs.Add(attr)
		}
		if isClient {
			desc.clientAttrs.Add(attr)
		}
		if isPersistent {
			desc.persistentAttrs.Add(attr)
		}
	}
}

type EntityManager struct {
	entities           EntityMap
	ownerOfClient      map[ClientID]EntityID
	registeredServices map[string]EntityIDSet
}

func newEntityManager() *EntityManager {
	return &EntityManager{
		entities:           EntityMap{},
		ownerOfClient:      map[ClientID]EntityID{},
		registeredServices: map[string]EntityIDSet{},
	}
}

func (em *EntityManager) put(entity *Entity) {
	em.entities.Add(entity)
}

func (em *EntityManager) del(entityID EntityID) {
	em.entities.Del(entityID)
}

func (em *EntityManager) get(id EntityID) *Entity {
	return em.entities.Get(id)
}

func (em *EntityManager) onEntityLoseClient(clientid ClientID) {
	delete(em.ownerOfClient, clientid)
}

func (em *EntityManager) onEntityGetClient(entityID EntityID, clientid ClientID) {
	em.ownerOfClient[clientid] = entityID
}

func (em *EntityManager) onClientDisconnected(clientid ClientID) {
	eid := em.ownerOfClient[clientid]
	if !eid.IsNil() { // should always true
		em.onEntityLoseClient(clientid)
		owner := em.get(eid)
		owner.notifyClientDisconnected()
	}
}

func (em *EntityManager) onGateDisconnected(gateid uint16) {
	for _, entity := range em.entities {
		client := entity.client
		if client != nil && client.gateid == gateid {
			em.onEntityLoseClient(client.clientid)
			entity.notifyClientDisconnected()
		}
	}
}

func (em *EntityManager) onDeclareService(serviceName string, eid EntityID) {
	eids, ok := em.registeredServices[serviceName]
	if !ok {
		eids = EntityIDSet{}
		em.registeredServices[serviceName] = eids
	}
	eids.Add(eid)
}

func (em *EntityManager) onUndeclareService(serviceName string, eid EntityID) {
	eids, ok := em.registeredServices[serviceName]
	if ok {
		eids.Del(eid)
	}
}

func (em *EntityManager) chooseServiceProvider(serviceName string) EntityID {
	// choose one entity ID of service providers randomly
	eids, ok := em.registeredServices[serviceName]
	if !ok {
		gwlog.Panicf("service not found: %s", serviceName)
	}

	r := rand.Intn(len(eids)) // get a random one
	for eid := range eids {
		if r == 0 {
			return eid
		}
		r -= 1
	}
	return "" // never goes here
}

func RegisterEntity(typeName string, entityPtr IEntity) *EntityTypeDesc {
	if _, ok := registeredEntityTypes[typeName]; ok {
		gwlog.Panicf("RegisterEntity: Entity type %s already registered", typeName)
	}
	entityVal := reflect.Indirect(reflect.ValueOf(entityPtr))
	entityType := entityVal.Type()

	// register the string of e
	rpcDescs := RpcDescMap{}
	entityTypeDesc := &EntityTypeDesc{
		entityType:      entityType,
		rpcDescs:        rpcDescs,
		clientAttrs:     StringSet{},
		allClientAttrs:  StringSet{},
		persistentAttrs: StringSet{},
	}
	registeredEntityTypes[typeName] = entityTypeDesc

	entityPtrType := reflect.PtrTo(entityType)
	numMethods := entityPtrType.NumMethod()
	for i := 0; i < numMethods; i++ {
		method := entityPtrType.Method(i)
		rpcDescs.visit(method)
	}

	gwlog.Debug(">>> RegisterEntity %s => %s <<<", typeName, entityType.Name())
	return entityTypeDesc
}

type createCause int

const (
	ccCreate createCause = 1 + iota
	ccMigrate
	ccRestore
)

func createEntity(typeName string, space *Space, pos Position, entityID EntityID, data map[string]interface{}, timerData []byte, client *GameClient, cause createCause) EntityID {
	//gwlog.Debug("createEntity: %s in Space %s", typeName, space)
	entityTypeDesc, ok := registeredEntityTypes[typeName]
	if !ok {
		gwlog.Panicf("unknown entity type: %s", typeName)
		if consts.DEBUG_MODE {
			os.Exit(2)
		}
	}

	if entityID == "" {
		entityID = GenEntityID()
	}

	var entity *Entity
	var entityInstance reflect.Value

	entityInstance = reflect.New(entityTypeDesc.entityType)
	entity = reflect.Indirect(entityInstance).FieldByName("Entity").Addr().Interface().(*Entity)
	entity.init(typeName, entityID, entityInstance)
	entity.Space = nilSpace

	entityManager.put(entity)
	if data != nil {
		if cause == ccCreate {
			entity.I.LoadPersistentData(data)
		} else {
			entity.I.LoadMigrateData(data)
		}
	} else {
		entity.Save() // save immediately after creation
	}

	if timerData != nil {
		entity.restoreTimers(timerData)
	}

	isPersistent := entity.I.IsPersistent()
	if isPersistent { // startup the periodical timer for saving e
		entity.setupSaveTimer()
	}

	if cause == ccCreate || cause == ccRestore {
		dispatcher_client.GetDispatcherClientForSend().SendNotifyCreateEntity(entityID)
	}

	if client != nil {
		// assign client to the newly created
		if cause == ccCreate {
			entity.SetClient(client)
		} else {
			entity.client = client // assign client quietly if migrate
			entityManager.onEntityGetClient(entity.ID, client.clientid)
		}
	}

	gwlog.Debug("Entity %s created, cause=%d, client=%s", entity, cause, client)
	if cause == ccCreate {
		gwutils.RunPanicless(entity.I.OnCreated)
	} else if cause == ccMigrate {
		gwutils.RunPanicless(entity.I.OnMigrateIn)
	} else if cause == ccRestore {
		// restore should be silent
		gwutils.RunPanicless(entity.I.OnRestored)
	}

	if space != nil {
		space.enter(entity, pos, cause == ccRestore)
	}

	return entityID
}

func loadEntityLocally(typeName string, entityID EntityID, space *Space, pos Position) {
	// load the data from storage
	storage.Load(typeName, entityID, func(data interface{}, err error) {
		// callback runs in main routine
		if err != nil {
			gwlog.Panicf("load entity %s.%s failed: %s", typeName, entityID, err)
			dispatcher_client.GetDispatcherClientForSend().SendNotifyDestroyEntity(entityID) // load entity failed, tell dispatcher
		}

		if space != nil && space.IsDestroyed() {
			// Space might be destroy during the Load process, so cancel the entity creation
			dispatcher_client.GetDispatcherClientForSend().SendNotifyDestroyEntity(entityID) // load entity failed, tell dispatcher
			return
		}

		createEntity(typeName, space, pos, entityID, data.(map[string]interface{}), nil, nil, ccCreate)
	})
}

func loadEntityAnywhere(typeName string, entityID EntityID) {
	dispatcher_client.GetDispatcherClientForSend().SendLoadEntityAnywhere(typeName, entityID)
}

func createEntityAnywhere(typeName string, data map[string]interface{}) {
	dispatcher_client.GetDispatcherClientForSend().SendCreateEntityAnywhere(typeName, data)
}

func CreateEntityLocally(typeName string, data map[string]interface{}, client *GameClient) EntityID {
	return createEntity(typeName, nil, Position{}, "", data, nil, client, ccCreate)
}

func CreateEntityAnywhere(typeName string) {
	createEntityAnywhere(typeName, nil)
}

func LoadEntityLocally(typeName string, entityID EntityID) {
	loadEntityLocally(typeName, entityID, nil, Position{})
}

func LoadEntityAnywhere(typeName string, entityID EntityID) {
	loadEntityAnywhere(typeName, entityID)
}

func OnClientDisconnected(clientid ClientID) {
	entityManager.onClientDisconnected(clientid) // pop the owner eid
}

func OnDeclareService(serviceName string, entityid EntityID) {
	entityManager.onDeclareService(serviceName, entityid)
}

func OnUndeclareService(serviceName string, entityid EntityID) {
	entityManager.onUndeclareService(serviceName, entityid)
}

func GetServiceProviders(serviceName string) EntityIDSet {
	return entityManager.registeredServices[serviceName]
}

func callEntity(id EntityID, method string, args []interface{}) {
	callRemote(id, method, args)

	// TODO: prohibit local call for test only, uncomment
	//e := entityManager.get(id)
	//if e != nil { // this entity is local, just call entity directly
	//	e.Post(func() { // TODO: what if the taret entity is migrating ? callRemote instead ?
	//		e.onCallFromLocal(method, args)
	//	})
	//} else {
	//	callRemote(id, method, args)
	//}
}

func callRemote(id EntityID, method string, args []interface{}) {
	dispatcher_client.GetDispatcherClientForSend().SendCallEntityMethod(id, method, args)
}

func OnCall(id EntityID, method string, args [][]byte, clientID ClientID) {
	e := entityManager.get(id)
	if e == nil {
		// entity not found, may destroyed before call
		gwlog.Error("Entity %s is not found while calling %s%v", id, method, args)
		return
	}

	e.onCallFromRemote(method, args, clientID)
}

func OnSyncPositionYawFromClient(eid EntityID, x, y, z Coord, yaw Yaw) {
	e := entityManager.get(eid)
	if e == nil {
		// entity not found, may destroyed before call
		gwlog.Error("OnSyncPositionYawFromClient: entity %s is not found", eid)
		return
	}

	e.syncPositionYawFromClient(x, y, z, yaw)
}

func GetEntity(id EntityID) *Entity {
	return entityManager.get(id)
}

func OnGameTerminating() {
	for _, e := range entityManager.entities {
		e.Destroy()
	}
}

func OnGateDisconnected(gateid uint16) {
	gwlog.Warn("Gate %d disconnected", gateid)
	entityManager.onGateDisconnected(gateid)
}

func SaveAllEntities() {
	for _, e := range entityManager.entities {
		e.Save()
	}
}

// Called by engine when server is freezing

type FreezeData struct {
	Entities map[EntityID]*entityFreezeData
	Services map[string][]EntityID
}

func Freeze(gameid uint16) (*FreezeData, error) {
	freeze := FreezeData{}

	entityFreezeInfos := map[EntityID]*entityFreezeData{}
	foundNilSpace := false
	for _, e := range entityManager.entities {
		entityFreezeInfos[e.ID] = e.GetFreezeData()
		if e.IsSpaceEntity() {
			if e.ToSpace().IsNil() {
				if foundNilSpace {
					return nil, errors.Errorf("found duplicate nil space")
				}
				foundNilSpace = true
			}
		}
	}

	if !foundNilSpace { // there should be exactly one nil space!
		return nil, errors.Errorf("nil space not found")
	}

	freeze.Entities = entityFreezeInfos
	registeredServices := make(map[string][]EntityID, len(entityManager.registeredServices))
	for serviceName, eids := range entityManager.registeredServices {
		registeredServices[serviceName] = eids.ToList()
	}
	freeze.Services = registeredServices

	return &freeze, nil
}

func RestoreFreezedEntities(freeze *FreezeData) (err error) {
	defer func() {
		_err := recover()
		if _err != nil {
			err = errors.Wrap(_err.(error), "panic during restore")
		}

	}()

	restoreEntities := func(filter func(typeName string, spaceKind int64) bool) {
		for eid, info := range freeze.Entities {
			typeName := info.Type
			var spaceKind int64
			if typeName == SPACE_ENTITY_TYPE {
				attrs := info.Attrs
				spaceKind = typeconv.Int(attrs[SPACE_KIND_ATTR_KEY])
			}

			if filter(typeName, spaceKind) {
				var space *Space
				if typeName != SPACE_ENTITY_TYPE {
					space = spaceManager.getSpace(info.SpaceID)
				}

				var client *GameClient
				if info.Client != nil {
					client = MakeGameClient(info.Client.ClientID, info.Client.GateID)
				}
				createEntity(typeName, space, info.Pos, eid, info.Attrs, info.TimerData, client, ccRestore)
				gwlog.Info("Restored %s<%s> in space %s", typeName, eid, space)

				if info.ESR != nil { // entity was entering space before freeze, so restore entering space
					post.Post(func() {
						entity := GetEntity(eid)
						if entity != nil {
							entity.EnterSpace(info.ESR.SpaceID, info.ESR.EnterPos)
						}
					})
				}
			}
		}
	}
	// step 1: restore the nil space
	restoreEntities(func(typeName string, spaceKind int64) bool {
		return typeName == SPACE_ENTITY_TYPE && spaceKind == 0
	})

	// step 2: restore all other spaces
	restoreEntities(func(typeName string, spaceKind int64) bool {
		return typeName == SPACE_ENTITY_TYPE && spaceKind != 0
	})

	// step  3: restore all other spaces
	restoreEntities(func(typeName string, spaceKind int64) bool {
		return typeName != SPACE_ENTITY_TYPE
	})

	for serviceName, _eids := range freeze.Services {
		eids := EntityIDSet{}
		for _, eid := range _eids {
			eids.Add(eid)
		}
		entityManager.registeredServices[serviceName] = eids
	}

	return nil
}

func Entities() EntityMap {
	return entityManager.entities
}
