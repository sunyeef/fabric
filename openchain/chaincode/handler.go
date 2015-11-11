/*
Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements.  See the NOTICE file
distributed with this work for additional information
regarding copyright ownership.  The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License.  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied.  See the License for the
specific language governing permissions and limitations
under the License.
*/

package chaincode

import (
	"fmt"
	"io"
	"sync"

	"github.com/golang/protobuf/proto"
	"github.com/looplab/fsm"
	"github.com/op/go-logging"
	pb "github.com/openblockchain/obc-peer/protos"
)

const (
	//FSM states
	CREATED_STATE		= "created"	//start state
	ESTABLISHED_STATE	= "established"	//in: CREATED, rcv:  REGISTER, send: REGISTERED, INIT
	INIT_STATE		= "init"	//in:ESTABLISHED, rcv:-, send: INIT
	READY_STATE		= "ready"	//in:ESTABLISHED,TRANSACTION, rcv:COMPLETED
	TRANSACTION_STATE	= "transaction"	//in:READY, rcv: xact from consensus, send: TRANSACTION
	BUSYINIT_STATE		= "busyinit"	//in:INIT, rcv: PUT_STATE, DEL_STATE, INVOKE_CHAINCODE 
	BUSYXACT_STATE		= "busyxact"	//in:TRANSACION, rcv: PUT_STATE, DEL_STATE, INVOKE_CHAINCODE
	END_STATE		= "end"		//in:INIT,ESTABLISHED, rcv: error, terminate container

)

var chaincodeLogger = logging.MustGetLogger("chaincode")

// PeerChaincodeStream interface for stream between Peer and chaincode instance.
type PeerChaincodeStream interface {
	Send(*pb.ChaincodeMessage) error
	Recv() (*pb.ChaincodeMessage, error)
}

// MessageHandler interface for handling chaincode messages (common between Peer chaincode support and chaincode)
type MessageHandler interface {
	HandleMessage(msg *pb.ChaincodeMessage) error
	SendMessage(msg *pb.ChaincodeMessage) error
}

// Handler responsbile for managment of Peer's side of chaincode stream
type Handler struct {
	sync.RWMutex
	ChatStream      PeerChaincodeStream
	FSM             *fsm.FSM
	ChaincodeID     *pb.ChainletID
	chainletSupport *ChainletSupport
	registered      bool
	readyNotify	chan bool
	responseNotifiers map[string] chan *pb.ChaincodeMessage
}

func (c *Handler) deregister() error {
	if c.registered {
		c.chainletSupport.deregisterHandler(c)
	}
	return nil
}

func (c *Handler) processStream() error {
	defer c.deregister()
	for {
		in, err := c.ChatStream.Recv()
		// Defer the deregistering of the this handler.
		if err == io.EOF {
			chainletLog.Debug("Received EOF, ending chaincode support stream")
			return err
		}
		if err != nil {
			chainletLog.Error(fmt.Sprintf("Error handling chaincode support stream: %s", err))
			return err
		}
		err = c.HandleMessage(in)
		if err != nil {
			return fmt.Errorf("Error handling message, ending stream: %s", err)
		}
	}
}

// HandleChaincodeStream Main loop for handling the associated Chaincode stream
func HandleChaincodeStream(chainletSupport *ChainletSupport, stream pb.ChainletSupport_RegisterServer) error {
	deadline, ok := stream.Context().Deadline()
	chainletLog.Debug("Current context deadline = %s, ok = %v", deadline, ok)
	handler := newChaincodeSupportHandler(chainletSupport, stream)
	return handler.processStream()
}

func newChaincodeSupportHandler(chainletSupport *ChainletSupport, peerChatStream PeerChaincodeStream) *Handler {
	v := &Handler{
		ChatStream: peerChatStream,
	}
	v.chainletSupport = chainletSupport

	v.FSM = fsm.NewFSM(
		CREATED_STATE,
		fsm.Events{
			//Send REGISTERED, then, if deploy { trigger INIT(via INIT) } else { trigger READY(via COMPLETED) }
			{Name: pb.ChaincodeMessage_REGISTER.String(), Src: []string{CREATED_STATE}, Dst: ESTABLISHED_STATE},
			{Name: pb.ChaincodeMessage_INIT.String(), Src: []string{ESTABLISHED_STATE}, Dst: INIT_STATE},
			{Name: pb.ChaincodeMessage_READY.String(), Src: []string{ESTABLISHED_STATE}, Dst: READY_STATE},
			{Name: pb.ChaincodeMessage_TRANSACTION.String(), Src: []string{READY_STATE}, Dst: TRANSACTION_STATE},
			{Name: pb.ChaincodeMessage_PUT_STATE.String(), Src: []string{TRANSACTION_STATE}, Dst: BUSYXACT_STATE},
			{Name: pb.ChaincodeMessage_DEL_STATE.String(), Src: []string{TRANSACTION_STATE}, Dst: BUSYXACT_STATE},
			{Name: pb.ChaincodeMessage_INVOKE_CHAINCODE.String(), Src: []string{TRANSACTION_STATE}, Dst: BUSYXACT_STATE},
			{Name: pb.ChaincodeMessage_PUT_STATE.String(), Src: []string{INIT_STATE}, Dst: BUSYINIT_STATE},
			{Name: pb.ChaincodeMessage_DEL_STATE.String(), Src: []string{INIT_STATE}, Dst: BUSYINIT_STATE},
			{Name: pb.ChaincodeMessage_INVOKE_CHAINCODE.String(), Src: []string{INIT_STATE}, Dst: BUSYINIT_STATE},
			{Name: pb.ChaincodeMessage_COMPLETED.String(), Src: []string{INIT_STATE,TRANSACTION_STATE}, Dst: READY_STATE}, 
			{Name: pb.ChaincodeMessage_GET_STATE.String(), Src: []string{INIT_STATE}, Dst: INIT_STATE},
			{Name: pb.ChaincodeMessage_GET_STATE.String(), Src: []string{BUSYINIT_STATE}, Dst: BUSYINIT_STATE},
			{Name: pb.ChaincodeMessage_GET_STATE.String(), Src: []string{TRANSACTION_STATE}, Dst: TRANSACTION_STATE},
			{Name: pb.ChaincodeMessage_GET_STATE.String(), Src: []string{BUSYXACT_STATE}, Dst: BUSYXACT_STATE},
			{Name: pb.ChaincodeMessage_ERROR.String(), Src: []string{INIT_STATE}, Dst: END_STATE},
			{Name: pb.ChaincodeMessage_ERROR.String(), Src: []string{TRANSACTION_STATE}, Dst: READY_STATE},
			{Name: pb.ChaincodeMessage_ERROR.String(), Src: []string{BUSYINIT_STATE}, Dst: INIT_STATE},
			{Name: pb.ChaincodeMessage_ERROR.String(), Src: []string{BUSYXACT_STATE}, Dst: TRANSACTION_STATE},
			{Name: pb.ChaincodeMessage_RESPONSE.String(), Src: []string{BUSYINIT_STATE}, Dst: INIT_STATE},
			{Name: pb.ChaincodeMessage_RESPONSE.String(), Src: []string{BUSYXACT_STATE}, Dst: TRANSACTION_STATE},
		},
		fsm.Callbacks{
			"before_" + pb.ChaincodeMessage_REGISTER.String(): func(e *fsm.Event) { v.beforeRegisterEvent(e, v.FSM.Current()) },
			"before_" + pb.ChaincodeMessage_COMPLETED.String(): func(e *fsm.Event) { v.beforeCompletedEvent(e, v.FSM.Current()) },
			"before_" + pb.ChaincodeMessage_INIT.String(): func(e *fsm.Event) { v.beforeInitState(e, v.FSM.Current()) },
			"enter_" + ESTABLISHED_STATE: func(e *fsm.Event) { v.enterEstablishedState(e, v.FSM.Current()) },
			"enter_" + READY_STATE: func(e *fsm.Event) { v.enterReadyState(e, v.FSM.Current()) },
			"enter_" + BUSYINIT_STATE: func(e *fsm.Event) { v.enterBusyInitState(e, v.FSM.Current()) },
			"enter_" + BUSYXACT_STATE: func(e *fsm.Event) { v.enterBusyXactState(e, v.FSM.Current()) },
			"enter_" + TRANSACTION_STATE: func(e *fsm.Event) { v.enterTransactionState(e, v.FSM.Current()) },
			"enter_" + END_STATE: func(e *fsm.Event) { v.enterEndState(e, v.FSM.Current()) },
		},
	)
	return v
}

func (c *Handler) notifyDuringStartup(val bool) {
	//if USER_RUNS_CC readyNotify will be nil
	if c.readyNotify != nil {
		c.readyNotify <- val
	}
}

func (c *Handler) beforeRegisterEvent(e *fsm.Event, state string) {
	chaincodeLogger.Debug("Received %s in state %s", e.Event, state)
	msg, ok := e.Args[0].(*pb.ChaincodeMessage)
	if !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	chainletID := &pb.ChainletID{}
	err := proto.Unmarshal(msg.Payload, chainletID)
	if err != nil {
		e.Cancel(fmt.Errorf("Error in received %s, could NOT unmarshal registration info: %s", pb.ChaincodeMessage_REGISTER, err))
		return
	}

	// Now register with the chainletSupport
	c.ChaincodeID = chainletID
	err = c.chainletSupport.registerHandler(c)
	if err != nil {
		c.notifyDuringStartup(false)
		e.Cancel(err)
		return
	}

	chainletLog.Debug("Got %s for chainldetID = %s, sending back %s", e.Event, chainletID, pb.ChaincodeMessage_REGISTERED)
	if err := c.ChatStream.Send(&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_REGISTERED}); err != nil {
		c.notifyDuringStartup(false)
		e.Cancel(fmt.Errorf("Error sending %s: %s", pb.ChaincodeMessage_REGISTERED, err))
		return
	}
	c.notifyDuringStartup(true)
}

func (c *Handler) notify(msg *pb.ChaincodeMessage) {
	c.Lock()
	defer c.Unlock()
	notfy := c.responseNotifiers[msg.Uuid]
	if notfy == nil {
		fmt.Printf("notifier Uuid:%s does not exist\n", msg.Uuid)
	} else {
		notfy<-msg
		fmt.Printf("notified Uuid:%s\n", msg.Uuid)
	}
}

func (c *Handler) beforeCompletedEvent(e *fsm.Event, state string) {
	chaincodeLogger.Debug("Received %s in state %s", e.Event, state)
	msg, ok := e.Args[0].(*pb.ChaincodeMessage)
	if !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	chaincodeLogger.Debug("beforeCompleted uuid:%s", msg.Uuid)
	// Now notify
	c.notify(msg)

	return
}

func (c *Handler) beforeInitState(e *fsm.Event, state string) {
	chainletLog.Debug("Before state %s.. notifying waiter that we are up", state)
	c.notifyDuringStartup(true)
}

func (c *Handler) enterEstablishedState(e *fsm.Event, state string) {
	chainletLog.Debug("(enterEstablishedState)Entered state %s", state)
}

func (c *Handler) enterReadyState(e *fsm.Event, state string) {
	chainletLog.Debug("(enterReadyState)Entered state %s", state)
}

func (c *Handler) enterBusyInitState(e *fsm.Event, state string) {
	chainletLog.Debug("(enterBusyInitState)Entered state %s", state)
}

func (c *Handler) enterBusyXactState(e *fsm.Event, state string) {
	chainletLog.Debug("(enterBusyXactState)Entered state %s", state)
}

func (c *Handler) enterTransactionState(e *fsm.Event, state string) {
	chainletLog.Debug("(enterTransactionState)Entered state %s", state)
}

func (c *Handler) enterEndState(e *fsm.Event, state string) {
	chainletLog.Debug("(enterEndState)Entered state %s", state)
}

//if initArgs is set (should be for "deploy" only) move to Init
//else move to ready
func (c *Handler) initOrReady(uuid string, f *string, initArgs []string) (chan *pb.ChaincodeMessage, error) {
	var event string
	var notfy chan *pb.ChaincodeMessage
	if f != nil || initArgs != nil {
		chainletLog.Debug("sending INIT")
		var f2 string
		if f != nil {
			f2 = *f
		}
		funcArgsMsg := &pb.ChainletMessage{Function: f2, Args: initArgs}
		payload, err := proto.Marshal(funcArgsMsg)
		if err != nil {
			return nil,err
		}
		notfy,err = c.createNotifier(uuid)
		if err != nil {
			return nil,err
		}
		ccMsg := &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_INIT, Payload: payload, Uuid: uuid}
		if err = c.ChatStream.Send(ccMsg); err != nil {
			notfy <- &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Payload: []byte(fmt.Sprintf("Error sending %s: %s", pb.ChaincodeMessage_INIT, err)), Uuid: uuid }
			return notfy, fmt.Errorf("Error sending %s: %s", pb.ChaincodeMessage_INIT, err)
		}
		event = pb.ChaincodeMessage_INIT.String()
	} else {
		chainletLog.Debug("sending READY")
		event = pb.ChaincodeMessage_READY.String()
		//TODO this is really cheating... I should really notify when the state moves to READY...
		//but this is an internal move(not from chaincode, so lets just do it for now)
		notfy <- &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR, Uuid: uuid }
	}
	err := c.FSM.Event(event)
	if err != nil {
		fmt.Printf("Err : %s\n", err)
	} else {
		fmt.Printf("Successful event initiation\n")
	}
	return notfy, err
}

// HandleMessage implementation of MessageHandler interface.  Peer's handling of Chaincode messages.
func (c *Handler) HandleMessage(msg *pb.ChaincodeMessage) error {
	chaincodeLogger.Debug("Handling ChaincodeMessage of type: %s in state %s", msg.Type, c.FSM.Current())
	if c.FSM.Cannot(msg.Type.String()) {
		return fmt.Errorf("Chaincode handler FSM cannot handle message (%s) with payload size (%d) while in state: %s", msg.Type.String(), len(msg.Payload), c.FSM.Current())
	}
	err := c.FSM.Event(msg.Type.String(), msg)

	return filterError(err)
}

// Filter the Errors to allow NoTransitionError and CanceledError to not propogate for cases where embedded Err == nil
func filterError(errFromFSMEvent error) error {
	if errFromFSMEvent != nil {
		if noTransitionErr, ok := errFromFSMEvent.(*fsm.NoTransitionError); ok {
			if noTransitionErr.Err != nil {
				// Only allow NoTransitionError's, all others are considered true error.
				return errFromFSMEvent
			}
			chaincodeLogger.Debug("Ignoring NoTransitionError: %s", noTransitionErr)
		}
		if canceledErr, ok := errFromFSMEvent.(*fsm.CanceledError); ok {
			if canceledErr.Err != nil {
				// Only allow NoTransitionError's, all others are considered true error.
				return canceledErr
				//t.Error("expected only 'NoTransitionError'")
			}
			chaincodeLogger.Debug("Ignoring CanceledError: %s", canceledErr)
		}
	}
	return nil
}

func (c *Handler) deleteNotifier(uuid string) {
	c.Lock()
	if c.responseNotifiers != nil {
		delete(c.responseNotifiers,uuid)
	}
	c.Unlock()
}
func (c *Handler) createNotifier(uuid string) (chan *pb.ChaincodeMessage, error) {
	if c.responseNotifiers == nil {
		return nil,fmt.Errorf("cannot create notifier for Uuid:%s", uuid)
	}
	c.Lock()
	defer c.Unlock()
	if c.responseNotifiers[uuid] != nil {
		return nil, fmt.Errorf("Uuid:%s exists", uuid)
	}
	c.responseNotifiers[uuid] = make(chan *pb.ChaincodeMessage, 1)
	return c.responseNotifiers[uuid],nil
}

func (c *Handler) sendExecuteMessage(msg *pb.ChaincodeMessage) (chan *pb.ChaincodeMessage, error) {
	notfy,err := c.createNotifier(msg.Uuid)
	if err != nil {
		return nil, err
	}
	if err := c.ChatStream.Send(msg); err != nil {
		c.deleteNotifier(msg.Uuid)
		return nil, fmt.Errorf("SendMessage error sending %s(%s)", msg.Uuid, err)
	}

	if msg.Type.String() == pb.ChaincodeMessage_TRANSACTION.String() {
		c.FSM.Event(msg.Type.String(), msg)
	}
	return notfy, nil
}

/****************
func (c *Handler) initEvent() (chan *pb.ChaincodeMessage, error) {
	if c.responseNotifiers == nil {
		return nil,fmt.Errorf("SendMessage called before registration for Uuid:%s", msg.Uuid)
	}
	var notfy chan *pb.ChaincodeMessage
	c.Lock()
	if c.responseNotifiers[msg.Uuid] != nil {
		c.Unlock()
		return nil, fmt.Errorf("SendMessage Uuid:%s exists", msg.Uuid)
	}
	//note the explicit use of buffer 1. We won't block if the receiver times outi and does not wait
	//for our response
	c.responseNotifiers[msg.Uuid] = make(chan *pb.ChaincodeMessage, 1)
	c.Unlock()

	if err := c.ChatStream.Send(msg); err != nil {
		deleteNotifier(msg.Uuid)
		return nil, fmt.Errorf("SendMessage error sending %s(%s)", msg.Uuid, err)
	}
	return notfy, nil
}
*******************/
