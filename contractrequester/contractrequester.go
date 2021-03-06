/*
 *    Copyright 2019 Insolar Technologies
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 */

package contractrequester

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/insolar/insolar/configuration"
	"github.com/insolar/insolar/core"
	"github.com/insolar/insolar/core/message"
	"github.com/insolar/insolar/core/reply"
	"github.com/insolar/insolar/instrumentation/inslogger"
	"github.com/insolar/insolar/instrumentation/instracer"
)

// ContractRequester helps to call contracts
type ContractRequester struct {
	MessageBus   core.MessageBus   `inject:""`
	PulseStorage core.PulseStorage `inject:""`
	ResultMutex  sync.Mutex
	ResultMap    map[uint64]chan *message.ReturnResults
	Sequence     uint64
}

// New creates new ContractRequester
func New() (*ContractRequester, error) {
	return &ContractRequester{
		ResultMap: make(map[uint64]chan *message.ReturnResults),
	}, nil
}

func (cr *ContractRequester) Start(ctx context.Context) error {
	cr.MessageBus.MustRegister(core.TypeReturnResults, cr.ReceiveResult)
	return nil
}

func randomUint64() uint64 {
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	if err != nil {
		panic(err)
	}

	return binary.LittleEndian.Uint64(buf)
}

// SendRequest makes synchronously call to method of contract by its ref without additional information
func (cr *ContractRequester) SendRequest(ctx context.Context, ref *core.RecordRef, method string, argsIn []interface{}) (core.Reply, error) {
	ctx, span := instracer.StartSpan(ctx, "SendRequest "+method)
	defer span.End()

	args, err := core.MarshalArgs(argsIn...)
	if err != nil {
		return nil, errors.Wrap(err, "[ ContractRequester::SendRequest ] Can't marshal")
	}

	bm := &message.BaseLogicMessage{
		Nonce: randomUint64(),
	}
	routResult, err := cr.CallMethod(ctx, bm, false, ref, method, args, nil)
	if err != nil {
		return nil, errors.Wrap(err, "[ ContractRequester::SendRequest ] Can't route call")
	}

	return routResult, nil
}

func (cr *ContractRequester) CallMethod(ctx context.Context, base core.Message, async bool, ref *core.RecordRef, method string, argsIn core.Arguments, mustPrototype *core.RecordRef) (core.Reply, error) {
	ctx, span := instracer.StartSpan(ctx, "ContractRequester.CallMethod "+method)
	defer span.End()

	baseMessage, ok := base.(*message.BaseLogicMessage)
	if !ok {
		return nil, errors.New("Wrong type for BaseMessage")
	}
	log := inslogger.FromContext(ctx)

	mb := core.MessageBusFromContext(ctx, cr.MessageBus)
	if mb == nil {
		log.Debug("Context doesn't provide MessageBus")
		mb = cr.MessageBus
	}

	var mode message.MethodReturnMode
	if async {
		mode = message.ReturnNoWait
	} else {
		mode = message.ReturnResult
	}

	msg := &message.CallMethod{
		BaseLogicMessage: *baseMessage,
		ReturnMode:       mode,
		ObjectRef:        *ref,
		Method:           method,
		Arguments:        argsIn,
	}
	if mustPrototype != nil {
		msg.ProxyPrototype = *mustPrototype
	}

	var seq uint64
	var ch chan *message.ReturnResults

	if !async {
		cr.ResultMutex.Lock()
		cr.Sequence++
		seq = cr.Sequence
		msg.Sequence = seq
		ch = make(chan *message.ReturnResults, 1)
		cr.ResultMap[seq] = ch

		cr.ResultMutex.Unlock()
	}

	res, err := mb.Send(ctx, msg, nil)

	if err != nil {
		return nil, errors.Wrap(err, "couldn't dispatch event")
	}

	r, ok := res.(*reply.RegisterRequest)
	if !ok {
		return nil, errors.New("Got not reply.RegisterRequest in reply for CallMethod")
	}

	if async {
		return res, nil
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(configuration.NewAPIRunner().Timeout)*time.Second)
	defer cancel()
	inslogger.FromContext(ctx).Debug("Waiting for Method results ref=", r.Request)

	var result *reply.CallMethod

	select {
	case ret := <-ch:
		inslogger.FromContext(ctx).Debug("Got Method results")
		if ret.Error != "" {
			return nil, errors.New(ret.Error)
		}
		retReply, ok := ret.Reply.(*reply.CallMethod)
		if !ok {
			return nil, errors.New("Reply is not CallMethod")
		}
		result = &reply.CallMethod{
			Request: r.Request,
			Result:  retReply.Result,
		}
	case <-ctx.Done():
		cr.ResultMutex.Lock()
		delete(cr.ResultMap, seq)
		cr.ResultMutex.Unlock()
		return nil, errors.New("canceled")
	}

	return result, nil
}

func (cr *ContractRequester) CallConstructor(ctx context.Context, base core.Message, async bool,
	prototype *core.RecordRef, to *core.RecordRef, method string,
	argsIn core.Arguments, saveAs int) (*core.RecordRef, error) {
	baseMessage, ok := base.(*message.BaseLogicMessage)
	if !ok {
		return nil, errors.New("Wrong type for BaseMessage")
	}

	mb := core.MessageBusFromContext(ctx, cr.MessageBus)
	if mb == nil {
		return nil, errors.New("No access to message bus")
	}

	msg := &message.CallConstructor{
		BaseLogicMessage: *baseMessage,
		PrototypeRef:     *prototype,
		ParentRef:        *to,
		Method:           method,
		Arguments:        argsIn,
		SaveAs:           message.SaveAs(saveAs),
	}

	var seq uint64
	var ch chan *message.ReturnResults
	if !async {
		cr.ResultMutex.Lock()

		cr.Sequence++
		seq = cr.Sequence
		msg.Sequence = seq
		ch = make(chan *message.ReturnResults, 1)
		cr.ResultMap[seq] = ch

		cr.ResultMutex.Unlock()
	}

	res, err := mb.Send(ctx, msg, nil)
	if err != nil {
		return nil, errors.Wrap(err, "couldn't save new object as delegate")
	}

	r, ok := res.(*reply.RegisterRequest)
	if !ok {
		return nil, errors.New("Got not reply.CallConstructor in reply for CallConstructor")
	}

	if async {
		return &r.Request, nil
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(configuration.NewAPIRunner().Timeout)*time.Second)
	defer cancel()
	inslogger.FromContext(ctx).Debug("Waiting for constructor results req=", r.Request, " seq=", seq)

	select {
	case ret := <-ch:
		inslogger.FromContext(ctx).Debug("Got Constructor results")
		if ret.Error != "" {
			return nil, errors.New(ret.Error)
		}
		return &r.Request, nil
	case <-ctx.Done():

		cr.ResultMutex.Lock()
		delete(cr.ResultMap, seq)
		cr.ResultMutex.Unlock()

		return nil, errors.New("canceled")
	}
}

func (cr *ContractRequester) ReceiveResult(ctx context.Context, parcel core.Parcel) (core.Reply, error) {
	msg, ok := parcel.Message().(*message.ReturnResults)
	if !ok {
		return nil, errors.New("ReceiveResult() accepts only message.ReturnResults")
	}

	ctx, span := instracer.StartSpan(ctx, "ContractRequester.ReceiveResult")
	defer span.End()

	cr.ResultMutex.Lock()
	defer cr.ResultMutex.Unlock()

	logger := inslogger.FromContext(ctx)
	c, ok := cr.ResultMap[msg.Sequence]
	if !ok {
		logger.Info("oops unwaited results seq=", msg.Sequence)
		return &reply.OK{}, nil
	}
	logger.Debug("Got wanted results seq=", msg.Sequence)

	c <- msg
	delete(cr.ResultMap, msg.Sequence)

	return &reply.OK{}, nil
}
