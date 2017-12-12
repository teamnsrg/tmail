package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/teamnsrg/tmail/message"
)

// QMessage represents a message in queue
type QMessage struct {
	sync.Mutex
	Id                      int64
	Uuid                    string // Unique ID common to all QMessage, representing the queued ID of the message
	MailFrom                string
	AuthUser                string // Si il y a eu authentification SMTP contient le login/user sert pour le routage
	RcptTo                  string
	MessageId               string
	Host                    string
	LastUpdate              time.Time
	AddedAt                 time.Time
	NextDeliveryScheduledAt time.Time
	Status                  uint32 // 0 delivery in progress, 1 to be discarded, 2 scheduled, 3 to be bounced
	DeliveryFailedCount     uint32
}

// Delete delete message from queue
func (q *QMessage) Delete() error {
	q.Lock()
	defer q.Unlock()
	var err error
	// remove from DB
	if err = DB.Delete(q).Error; err != nil {
		return err
	}
	// If there is no other reference in DB, remove raw message from store
	var c uint
	if err = DB.Model(QMessage{}).Where("`uuid` = ?", q.Uuid).Count(&c).Error; err != nil {
		return err
	}
	if c != 0 {
		return nil
	}
	/*qStore, err := NewStore(Cfg.GetStoreDriver(), Cfg.GetStoreSource())
	if err != nil {
		return err
	}*/
	err = Store.Del(q.Uuid)
	// Si le fichier n'existe pas ce n'est pas une véritable erreur
	if err != nil && strings.Contains(err.Error(), "no such file") {
		err = nil
	}
	return err
}

// UpdateFromDb update message from DB
func (q *QMessage) UpdateFromDb() error {
	q.Lock()
	defer q.Unlock()
	return DB.First(q, q.Id).Error
}

// SaveInDb save qMessage in DB
func (q *QMessage) SaveInDb() error {
	q.Lock()
	defer q.Unlock()
	q.LastUpdate = time.Now()
	return DB.Save(q).Error
}

// Discard mark message as being discarded on next delivery attemp
func (q *QMessage) Discard() error {
	if q.Status == 0 {
		return errors.New("delivery in progress, message status can't be changed")
	}
	q.Lock()
	q.Status = 1
	q.Unlock()
	return q.SaveInDb()
}

// Bounce mark message as being bounced on next delivery attemp
func (q *QMessage) Bounce() error {
	if q.Status == 0 {
		return errors.New("delivery in progress, message status can't be changed")
	}
	q.Lock()
	q.Status = 3
	q.Unlock()
	return q.SaveInDb()
}

// QueueGetMessageById return a message from is key
func QueueGetMessageById(id int64) (msg QMessage, err error) {
	msg = QMessage{}
	err = DB.Where("id = ?", id).First(&msg).Error
	/*if err != nil && err == gorm.RecordNotFound {
		err = errors.New("not found")
	}*/
	return
}

// QueueGetExpiredMessages return expired messages from DB
func QueueGetExpiredMessages() (messages []QMessage, err error) {
	messages = []QMessage{}
	from := time.Now().Add(-24 * time.Hour)
	err = DB.Where("next_delivery_scheduled_at < ?", from).Find(&messages).Error
	return
}

// QueueAddMessage add a new mail in queue
func QueueAddMessage(rawMess *[]byte, envelope message.Envelope, authUser string) (uuid string, err error) {
	qStore, err := NewStore(Cfg.GetStoreDriver(), Cfg.GetStoreSource())
	if err != nil {
		return
	}

	uuid, err = NewUUID()
	if err != nil {
		return
	}
	err = qStore.Put(uuid, bytes.NewReader(*rawMess))
	if err != nil {
		return
	}

	messageId := message.RawGetMessageId(rawMess)

	cloop := 0
	qmessages := []QMessage{}
	for _, rcptTo := range envelope.RcptTo {
		qm := QMessage{
			Uuid:                    uuid,
			AuthUser:                authUser,
			MailFrom:                envelope.MailFrom,
			RcptTo:                  rcptTo,
			MessageId:               string(messageId),
			Host:                    message.GetHostFromAddress(rcptTo),
			LastUpdate:              time.Now(),
			AddedAt:                 time.Now(),
			NextDeliveryScheduledAt: time.Now(),
			Status:                  2,
			DeliveryFailedCount:     0,
		}

		// create record in db
		err = DB.Create(&qm).Error
		if err != nil {
			if cloop == 0 {
				qStore.Del(uuid)
			}
			return
		}
		cloop++
		qmessages = append(qmessages, qm)
	}

	// publish qmessage
	// TODO: to avoid the copy of the Lock -> qmsg.Publish()
	for _, qmsg := range qmessages {
		var jMsg []byte
		jMsg, err = json.Marshal(qmsg)
		if err != nil {
			if cloop == 1 {
				qStore.Del(uuid)
			}
			DB.Delete(&qmsg)
			return
		}
		// queue local  | queue remote
		err = NsqQueueProducer.Publish("todeliver", jMsg)
		if err != nil {
			if cloop == 1 {
				qStore.Del(uuid)
			}
			DB.Delete(&qmsg)
			return
		}
	}
	return
}

// QueueListMessages return all messages in queue
func QueueListMessages() ([]QMessage, error) {
	messages := []QMessage{}
	err := DB.Find(&messages).Error
	return messages, err
}

// QueueCount rerurn the number of message in queue
func QueueCount() (c uint32, err error) {
	c = 0
	err = DB.Model(QMessage{}).Count(&c).Error
	return
}
