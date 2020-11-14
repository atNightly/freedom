package domainevent

import (
	"errors"
	"fmt"
	"time"

	"github.com/8treenet/freedom"
	"github.com/8treenet/freedom/infra/kafka"
	"github.com/jinzhu/gorm"
)

var eventManager *EventManager

func init() {
	eventManager = &EventManager{}
	freedom.Prepare(func(initiator freedom.Initiator) {
		initiator.BindInfra(true, eventManager) //单例绑定
		initiator.InjectController(func(ctx freedom.Context) (com *EventManager) {
			initiator.GetInfra(ctx, &com)
			return
		})
	})
}

// GetEventManager .
func GetEventManager() *EventManager {
	return eventManager
}

// EventManager .
type EventManager struct {
	freedom.Infra
	kafkaProducer *kafka.ProducerImpl //Kafka Producer组件
}

// Booting .
func (manager *EventManager) Booting(sb freedom.SingleBoot) {
	if err := manager.db().AutoMigrate(&domainEventPublish{}).Error; err != nil {
		panic(err)
	}
	if err := manager.db().AutoMigrate(&domainEventSubscribe{}).Error; err != nil {
		panic(err)
	}

	//获取Kafka Producer组件, 只支持单例组件的获取.
	if !manager.GetSingleInfra(&manager.kafkaProducer) {
		panic("getkafkaProducer")
	}
}

// push EventTransaction在事务成功后触发 .
func (manager *EventManager) push(event freedom.DomainEvent) {
	freedom.Logger().Infof("领域事件发布 Topic:%s, %+v", event.Topic(), event)
	eventID := event.Identity().(int)
	go func() {
		/*
			Kafka 消息模式参考 example/infra-example
			if err := manager.kafkaProducer.NewMsg(event.Topic(), event.Marshal()).SetHeader(event.GetPrototypes()).Publish(); err != nil {
				freedom.Logger().Error(err)
				return
			}

			REST 模式参考 example/http2
			manager.NewHTTPRequest(URL)
			manager.NewH2CRequest(URL)
		*/

		msg := manager.kafkaProducer.NewMsg(event.Topic(), event.Marshal()).SetHeader(event.GetPrototypes())
		msg.SetMessageKey(fmt.Sprint(eventID)) //设置kafka消息key

		//消息发送
		if err := msg.Publish(); err != nil {
			freedom.Logger().Error(err)
			return
		}

		publish := &domainEventPublish{ID: eventID}
		// Push成功后删除事件
		if err := manager.db().Delete(&publish).Error; err != nil {
			freedom.Logger().Error(err)
		}
	}()
}

func (manager *EventManager) db() *gorm.DB {
	return manager.SourceDB().(*gorm.DB)
}

// Save .
func (manager *EventManager) Save(repo *freedom.Repository, entity freedom.Entity) (e error) {
	txDB := repoDB(repo) //获取事务db
	pubEvents := entity.GetPubEvent()
	subEvents := entity.GetSubEvent()

	//删除实体里的全部事件
	entity.RemoveAllPubEvent()
	entity.RemoveAllSubEvent()

	//Insert PubEvent
	for _, domainEvent := range pubEvents {
		model := domainEventPublish{
			Topic:   domainEvent.Topic(),
			Content: string(domainEvent.Marshal()),
			Created: time.Now(),
			Updated: time.Now(),
		}
		e = txDB.Create(&model).Error //插入生产事件表。
		if e != nil {
			return
		}
		domainEvent.SetIdentity(model.ID)
	}
	manager.addPubToWorker(repo.GetWorker(), pubEvents)

	//Update SubEvent
	for _, subEvent := range subEvents {
		eventID := subEvent.Identity().(int)
		subscribe := &domainEventSubscribe{PublishID: eventID}
		subscribe.SetSuccess(1)                                             //状态已处理
		rowResult := txDB.Model(subscribe).Updates(subscribe.TakeChanges()) //修改消费事件表

		e = rowResult.Error
		if e != nil {
			freedom.Logger().Error(rowResult.Error)
			return
		}
		if rowResult.RowsAffected == 0 {
			e = errors.New("Event not found")
			return
		}
	}
	return
}

// InsertSubEvent .
func (manager *EventManager) InsertSubEvent(event freedom.DomainEvent) error {
	model := domainEventSubscribe{
		PublishID: event.Identity().(int),
		Topic:     event.Topic(),
		Content:   string(event.Marshal()),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	err := manager.db().Create(&model).Error //插入消费事件表。
	if err != nil {
		return err
	}
	return nil
}

// Retry .
func (manager *EventManager) Retry() {
	//定时器扫描表中失败的Pub/Sub事件
	freedom.Logger().Info("EventManager Retry")
}

// addPubToWorker 增加发布事件到worker的Store.
func (manager *EventManager) addPubToWorker(worker freedom.Worker, pubs []freedom.DomainEvent) {
	if len(pubs) == 0 {
		return
	}
	//把worker的请求信息 写到事件的属性里
	m := map[string]interface{}{}
	for key, item := range worker.Bus().Header {
		if len(item) <= 0 {
			continue
		}
		m[key] = item[0]
	}
	for _, pubEvent := range pubs {
		pubEvent.SetPrototypes(m)
	}

	//把发布事件加到store内, EventTransaction在事务结束后会触发push
	var storePubEvents []freedom.DomainEvent
	store := worker.Store().Get(workerStorePubEventKey)
	if store != nil {
		list, ok := store.([]freedom.DomainEvent)
		if ok {
			storePubEvents = list
		}
	}
	storePubEvents = append(storePubEvents, pubs...)
	worker.Store().Set(workerStorePubEventKey, storePubEvents)
}

// repoDB 获取请求内事务的DB
func repoDB(repo *freedom.Repository) *gorm.DB {
	var db *gorm.DB
	if err := repo.FetchDB(&db); err != nil {
		panic(err)
	}
	db = db.New()
	db.SetLogger(repo.Worker.Logger())
	return db
}
