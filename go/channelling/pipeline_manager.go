/*
 * Spreed WebRTC.
 * Copyright (C) 2013-2015 struktur AG
 *
 * This file is part of Spreed WebRTC.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package channelling

import (
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	PipelineNamespaceCall = "call"
)

type PipelineManager interface {
	BusManager
	SessionStore
	UserStore
	SessionCreator
	GetPipelineByID(id string) (pipeline *Pipeline, ok bool)
	GetPipeline(namespace string, sender Sender, session *Session, to string) *Pipeline
	FindSink(to string) Sink
}

type pipelineManager struct {
	BusManager
	SessionStore
	UserStore
	SessionCreator
	mutex               sync.RWMutex
	pipelineTable       map[string]*Pipeline
	sessionTable        map[string]*Session
	sessionByBusIDTable map[string]*Session
	sessionSinkTable    map[string]Sink
	duration            time.Duration
}

func NewPipelineManager(busManager BusManager, sessionStore SessionStore, userStore UserStore, sessionCreator SessionCreator) PipelineManager {
	plm := &pipelineManager{
		BusManager:          busManager,
		SessionStore:        sessionStore,
		UserStore:           userStore,
		SessionCreator:      sessionCreator,
		pipelineTable:       make(map[string]*Pipeline),
		sessionTable:        make(map[string]*Session),
		sessionByBusIDTable: make(map[string]*Session),
		sessionSinkTable:    make(map[string]Sink),
		duration:            30 * time.Minute,
	}
	plm.start()

	plm.Subscribe("channelling.session.create", plm.sessionCreate)
	plm.Subscribe("channelling.session.close", plm.sessionClose)

	return plm
}

func (plm *pipelineManager) cleanup() {
	plm.mutex.Lock()
	for id, pipeline := range plm.pipelineTable {
		if pipeline.Expired() {
			pipeline.Close()
			delete(plm.pipelineTable, id)
		}
	}
	plm.mutex.Unlock()
}

func (plm *pipelineManager) start() {
	c := time.Tick(30 * time.Second)
	go func() {
		for _ = range c {
			plm.cleanup()
		}
	}()
}

func (plm *pipelineManager) sessionCreate(subject, reply string, msg *SessionCreateRequest) {
	log.Println("sessionCreate via NATS", subject, reply, msg)

	if msg.Session == nil || msg.Id == "" {
		return
	}

	var sink Sink

	plm.mutex.Lock()
	session, ok := plm.sessionByBusIDTable[msg.Id]
	if ok {
		// Remove existing session with same ID.
		delete(plm.sessionTable, session.Id)
		sink, _ = plm.sessionSinkTable[session.Id]
		delete(plm.sessionSinkTable, session.Id)
		session.Close()
		if sink != nil {
			sink.Close()
		}
	}
	session = plm.CreateSession(nil, "")
	plm.sessionByBusIDTable[msg.Id] = session
	plm.sessionTable[session.Id] = session
	if sink == nil {
		sink = plm.CreateSink(msg.Id)
	}
	plm.sessionSinkTable[session.Id] = sink
	plm.mutex.Unlock()

	session.Status = msg.Session.Status
	session.SetUseridFake(msg.Session.Userid)
	//pipeline := plm.GetPipeline("", nil, session, "")

	if msg.Room != nil {
		room, err := session.JoinRoom(msg.Room.Name, msg.Room.Type, msg.Room.Credentials, nil)
		log.Println("Joined NATS session to room", room, err)
	}

	session.BroadcastStatus()
}

func (plm *pipelineManager) sessionClose(subject, reply string, id string) {
	log.Println("sessionClose via NATS", subject, reply, id)

	if id == "" {
		return
	}

	plm.mutex.Lock()
	session, ok := plm.sessionByBusIDTable[id]
	if ok {
		delete(plm.sessionByBusIDTable, id)
		delete(plm.sessionTable, session.Id)
		if sink, ok := plm.sessionSinkTable[session.Id]; ok {
			delete(plm.sessionSinkTable, session.Id)
			sink.Close()
		}
	}
	plm.mutex.Unlock()

	if ok {
		session.Close()
	}
}

func (plm *pipelineManager) GetPipelineByID(id string) (*Pipeline, bool) {
	plm.mutex.RLock()
	pipeline, ok := plm.pipelineTable[id]
	if !ok {
		// XXX(longsleep): Hack for development
		for _, pipeline = range plm.pipelineTable {
			ok = true
			break
		}
	}
	plm.mutex.RUnlock()
	return pipeline, ok
}

func (plm *pipelineManager) PipelineID(namespace string, sender Sender, session *Session, to string) string {
	return fmt.Sprintf("%s.%s.%s", namespace, session.Id, to)
}

func (plm *pipelineManager) GetPipeline(namespace string, sender Sender, session *Session, to string) *Pipeline {
	id := plm.PipelineID(namespace, sender, session, to)

	plm.mutex.Lock()
	pipeline, ok := plm.pipelineTable[id]
	if ok {
		// Refresh. We do not care if the pipeline is expired.
		pipeline.Refresh(plm.duration)
		plm.mutex.Unlock()
		return pipeline
	}

	log.Println("Creating pipeline", namespace, id)
	pipeline = NewPipeline(plm, namespace, id, session, plm.duration)
	plm.pipelineTable[id] = pipeline
	plm.mutex.Unlock()

	return pipeline
}

func (plm *pipelineManager) FindSink(to string) Sink {
	// It is possible to retrieve the userid for fake sessions here.
	plm.mutex.RLock()
	if sink, found := plm.sessionSinkTable[to]; found {
		plm.mutex.RUnlock()
		if sink.Enabled() {
			log.Println("Pipeline sink found via manager", sink)
			return sink
		}
		return nil
	}

	plm.mutex.RUnlock()
	return nil
}
