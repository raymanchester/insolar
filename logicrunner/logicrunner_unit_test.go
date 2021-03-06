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

package logicrunner

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gojuno/minimock"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/insolar/insolar/configuration"
	"github.com/insolar/insolar/core"
	"github.com/insolar/insolar/core/message"
	"github.com/insolar/insolar/core/reply"
	"github.com/insolar/insolar/instrumentation/inslogger"
	"github.com/insolar/insolar/testutils"
	"github.com/insolar/insolar/testutils/network"
)

type LogicRunnerCommonTestSuite struct {
	suite.Suite

	mc  *minimock.Controller
	ctx context.Context
	am  *testutils.ArtifactManagerMock
	mb  *testutils.MessageBusMock
	jc  *testutils.JetCoordinatorMock
	lr  *LogicRunner
	es  ExecutionState
	ps  *testutils.PulseStorageMock
	mle *testutils.MachineLogicExecutorMock
	nn  *network.NodeNetworkMock
}

func (suite *LogicRunnerCommonTestSuite) BeforeTest(suiteName, testName string) {
	// testing context
	suite.ctx = inslogger.TestContext(suite.T())

	// initialize minimock and mocks
	suite.mc = minimock.NewController(suite.T())
	suite.am = testutils.NewArtifactManagerMock(suite.mc)
	suite.mb = testutils.NewMessageBusMock(suite.mc)
	suite.jc = testutils.NewJetCoordinatorMock(suite.mc)
	suite.ps = testutils.NewPulseStorageMock(suite.mc)
	suite.nn = network.NewNodeNetworkMock(suite.mc)

	suite.SetupLogicRunner()
}

func (suite *LogicRunnerCommonTestSuite) SetupLogicRunner() {
	suite.lr, _ = NewLogicRunner(&configuration.LogicRunner{})
	suite.lr.ArtifactManager = suite.am
	suite.lr.MessageBus = suite.mb
	suite.lr.JetCoordinator = suite.jc
	suite.lr.PulseStorage = suite.ps
	suite.lr.NodeNetwork = suite.nn
}

func (suite *LogicRunnerCommonTestSuite) AfterTest(suiteName, testName string) {
	suite.mc.Wait(10 * time.Second)
	suite.mc.Finish()
}

type LogicRunnerTestSuite struct {
	LogicRunnerCommonTestSuite
}

func (suite *LogicRunnerTestSuite) BeforeTest(suiteName, testName string) {
	suite.LogicRunnerCommonTestSuite.BeforeTest(suiteName, testName)
}

func (suite *LogicRunnerTestSuite) SetupLogicRunner() {
	suite.LogicRunnerCommonTestSuite.SetupLogicRunner()
}

func (suite *LogicRunnerTestSuite) AfterTest(suiteName, testName string) {
	suite.LogicRunnerCommonTestSuite.AfterTest(suiteName, testName)
}

func (suite *LogicRunnerTestSuite) TestPendingFinished() {
	pulse := core.Pulse{}
	objectRef := testutils.RandomRef()
	meRef := testutils.RandomRef()

	suite.jc.MeMock.Return(meRef)
	suite.ps.CurrentMock.Return(&pulse, nil)

	es := &ExecutionState{
		Ref:       objectRef,
		Behaviour: &ValidationSaver{},
		Current:   &CurrentExecution{},
		pending:   message.NotPending,
	}

	// make sure that if there is no pending finishPendingIfNeeded returns false,
	// doesn't send PendingFinished message and doesn't change ExecutionState.pending
	suite.lr.finishPendingIfNeeded(suite.ctx, es)
	suite.Require().Zero(suite.mb.SendCounter)
	suite.Require().Equal(message.NotPending, es.pending)

	es.pending = message.InPending
	es.objectbody = &ObjectBody{}
	suite.mb.SendMock.ExpectOnce(suite.ctx, &message.PendingFinished{Reference: objectRef}, nil).Return(&reply.ID{}, nil)
	suite.jc.IsAuthorizedMock.Return(false, nil)
	suite.lr.finishPendingIfNeeded(suite.ctx, es)
	suite.Require().Equal(message.NotPending, es.pending)
	suite.Require().Nil(es.objectbody)

	suite.mc.Wait(time.Second) // message bus' send is called in a goroutine

	es.pending = message.InPending
	es.objectbody = &ObjectBody{}
	suite.jc.IsAuthorizedMock.Return(true, nil)
	suite.lr.finishPendingIfNeeded(suite.ctx, es)
	suite.Require().Equal(message.NotPending, es.pending)
	suite.Require().NotNil(es.objectbody)
}

func (suite *LogicRunnerTestSuite) TestStartQueueProcessorIfNeeded_DontStartQueueProcessorWhenPending() {
	es := &ExecutionState{Queue: make([]ExecutionQueueElement, 0), pending: message.InPending}
	es.Queue = append(es.Queue, ExecutionQueueElement{})
	err := suite.lr.StartQueueProcessorIfNeeded(
		suite.ctx, es,
	)
	suite.Require().NoError(err)
	suite.Require().Equal(message.InPending, es.pending)
}

func (suite *LogicRunnerTestSuite) TestCheckPendingRequests() {
	objectRef := testutils.RandomRef()

	table := []struct {
		name        string
		inState     message.PendingState
		outState    message.PendingState
		message     bool
		messageType core.MessageType
		amReply     *struct {
			has bool;
			err error
		}
		isError bool
	}{
		{
			name:     "already in pending",
			inState:  message.InPending,
			outState: message.InPending,
		},
		{
			name:     "already not in pending",
			inState:  message.NotPending,
			outState: message.NotPending,
		},
		{
			name:        "constructor call",
			inState:     message.PendingUnknown,
			message:     true,
			messageType: core.TypeCallConstructor,
			outState:    message.NotPending,
		},
		{
			name:        "method call, not pending",
			inState:     message.PendingUnknown,
			message:     true,
			messageType: core.TypeCallMethod,
			amReply: &struct {
				has bool;
				err error
			}{false, nil},
			outState: message.NotPending,
		},
		{
			name:        "method call, in pending",
			inState:     message.PendingUnknown,
			message:     true,
			messageType: core.TypeCallMethod,
			amReply: &struct {
				has bool;
				err error
			}{true, nil},
			outState: message.InPending,
		},
		{
			name:        "method call, in pending",
			inState:     message.PendingUnknown,
			message:     true,
			messageType: core.TypeCallMethod,
			amReply: &struct {
				has bool;
				err error
			}{true, errors.New("some")},
			outState: message.PendingUnknown,
			isError:  true,
		},
	}

	for _, test := range table {
		suite.T().Run(test.name, func(t *testing.T) {
			parcel := testutils.NewParcelMock(t)
			if test.message {
				parcel.TypeMock.ExpectOnce().Return(test.messageType)
			}
			es := &ExecutionState{Ref: objectRef, pending: test.inState}
			if test.amReply != nil {
				suite.am.HasPendingRequestsMock.Return(test.amReply.has, test.amReply.err)
			}
			err := suite.lr.ClarifyPendingState(suite.ctx, es, parcel)
			if test.isError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, test.outState, es.pending)
		})
	}

	suite.T().Run("method call, AM error", func(t *testing.T) {
		parcel := testutils.NewParcelMock(t)
		parcel.TypeMock.Expect().Return(core.TypeCallMethod)
		es := &ExecutionState{Ref: objectRef, pending: message.PendingUnknown}
		suite.am.HasPendingRequestsMock.Return(false, errors.New("some"))
		err := suite.lr.ClarifyPendingState(suite.ctx, es, parcel)
		require.Error(t, err)
		require.Equal(t, message.PendingUnknown, es.pending)
	})
}

func (suite *LogicRunnerTestSuite) TestPrepareState() {
	object := testutils.RandomRef()
	msg := &message.ExecutorResults{
		Caller:    testutils.RandomRef(),
		RecordRef: object,
	}

	// not pending
	// it's a first call, it's also initialize lr.state[object].ExecutionState
	// also check for empty Queue
	msg.Pending = message.NotPending
	_ = suite.lr.prepareObjectState(suite.ctx, msg)
	suite.Require().Equal(message.NotPending, suite.lr.state[object].ExecutionState.pending)
	suite.Require().Equal(0, len(suite.lr.state[object].ExecutionState.Queue))

	// pending without queue
	suite.lr.state[object].ExecutionState.pending = message.PendingUnknown
	msg.Pending = message.InPending
	_ = suite.lr.prepareObjectState(suite.ctx, msg)
	suite.Require().Equal(message.InPending, suite.lr.state[object].ExecutionState.pending)

	// do not change pending status if it isn't unknown
	suite.lr.state[object].ExecutionState.pending = message.NotPending
	msg.Pending = message.InPending
	_ = suite.lr.prepareObjectState(suite.ctx, msg)
	suite.Require().Equal(message.NotPending, suite.lr.state[object].ExecutionState.pending)

	// do not change pending status if it isn't unknown
	suite.lr.state[object].ExecutionState.pending = message.InPending
	msg.Pending = message.InPending
	_ = suite.lr.prepareObjectState(suite.ctx, msg)
	suite.Require().Equal(message.InPending, suite.lr.state[object].ExecutionState.pending)

	parcel := testutils.NewParcelMock(suite.mc)
	parcel.ContextMock.Expect(context.Background()).Return(context.Background())
	// brand new queue from message
	msg.Queue = []message.ExecutionQueueElement{{Parcel: parcel}}
	_ = suite.lr.prepareObjectState(suite.ctx, msg)
	suite.Require().Equal(1, len(suite.lr.state[object].ExecutionState.Queue))

	testMsg := message.CallMethod{ReturnMode: message.ReturnNoWait}
	parcel = testutils.NewParcelMock(suite.mc)
	parcel.ContextMock.Expect(context.Background()).Return(context.Background())
	parcel.MessageMock.Return(&testMsg) // mock message that returns NoWait

	queueElementRequest := testutils.RandomRef()
	msg.Queue = []message.ExecutionQueueElement{{Request: &queueElementRequest, Parcel: parcel}}
	_ = suite.lr.prepareObjectState(suite.ctx, msg)
	suite.Require().Equal(2, len(suite.lr.state[object].ExecutionState.Queue))
	suite.Require().Equal(&queueElementRequest, suite.lr.state[object].ExecutionState.Queue[0].request)
	suite.Require().Equal(&testMsg, suite.lr.state[object].ExecutionState.Queue[0].parcel.Message())
}

func (suite *LogicRunnerTestSuite) TestHandlePendingFinishedMessage() {
	objectRef := testutils.RandomRef()

	parcel := testutils.NewParcelMock(suite.mc).MessageMock.Return(
		&message.PendingFinished{Reference: objectRef},
	)

	parcel.DefaultTargetMock.Return(&core.RecordRef{})

	re, err := suite.lr.HandlePendingFinishedMessage(suite.ctx, parcel)
	suite.Require().NoError(err)
	suite.Require().Equal(&reply.OK{}, re)

	st := suite.lr.MustObjectState(objectRef)

	es := st.ExecutionState
	suite.Require().NotNil(es)
	suite.Require().Equal(message.NotPending, es.pending)

	es.Current = &CurrentExecution{}
	re, err = suite.lr.HandlePendingFinishedMessage(suite.ctx, parcel)
	suite.Require().Error(err)

	es.Current = nil

	re, err = suite.lr.HandlePendingFinishedMessage(suite.ctx, parcel)
	suite.Require().NoError(err)
	suite.Require().Equal(&reply.OK{}, re)
}

func (suite *LogicRunnerTestSuite) TestCheckExecutionLoop() {
	es := &ExecutionState{
		Current: nil,
	}

	loop := suite.lr.CheckExecutionLoop(suite.ctx, es, nil)
	suite.Require().False(loop)

	ctxA, _ := inslogger.WithTraceField(suite.ctx, "a")
	ctxB, _ := inslogger.WithTraceField(suite.ctx, "b")

	parcel := testutils.NewParcelMock(suite.mc).MessageMock.Return(
		&message.CallMethod{ReturnMode: message.ReturnResult},
	)
	es.Current = &CurrentExecution{
		ReturnMode: message.ReturnResult,
		Context:    ctxA,
	}

	loop = suite.lr.CheckExecutionLoop(ctxA, es, parcel)
	suite.Require().True(loop)

	loop = suite.lr.CheckExecutionLoop(ctxB, es, parcel)
	suite.Require().False(loop)

	parcel = testutils.NewParcelMock(suite.mc).MessageMock.Return(
		&message.CallMethod{ReturnMode: message.ReturnNoWait},
	)
	es.Current = &CurrentExecution{
		ReturnMode: message.ReturnResult,
		Context:    ctxA,
	}
	loop = suite.lr.CheckExecutionLoop(ctxA, es, parcel)
	suite.Require().False(loop)

	parcel = testutils.NewParcelMock(suite.mc)
	es.Current = &CurrentExecution{
		ReturnMode: message.ReturnNoWait,
		Context:    ctxA,
	}
	loop = suite.lr.CheckExecutionLoop(ctxA, es, parcel)
	suite.Require().False(loop)

	es.Current = &CurrentExecution{
		ReturnMode: message.ReturnNoWait,
		Context:    ctxA,
		SentResult: true,
	}
	loop = suite.lr.CheckExecutionLoop(ctxA, es, parcel)
	suite.Require().False(loop)
}

func (suite *LogicRunnerTestSuite) TestHandleStillExecutingMessage() {
	objectRef := testutils.RandomRef()

	parcel := testutils.NewParcelMock(suite.mc).MessageMock.Return(
		&message.StillExecuting{Reference: objectRef},
	)

	parcel.DefaultTargetMock.Return(&core.RecordRef{})

	// check that creation of new execution state is handled (on StillExecuting Message)
	re, err := suite.lr.HandleStillExecutingMessage(suite.ctx, parcel)
	suite.Require().NoError(err)
	suite.Require().Equal(&reply.OK{}, re)

	st := suite.lr.MustObjectState(objectRef)
	suite.Require().NotNil(st.ExecutionState)
	suite.Require().Equal(message.InPending, st.ExecutionState.pending)
	suite.Require().Equal(true, st.ExecutionState.PendingConfirmed)

	st.ExecutionState.pending = message.NotPending
	st.ExecutionState.PendingConfirmed = false

	re, err = suite.lr.HandleStillExecutingMessage(suite.ctx, parcel)
	suite.Require().NoError(err)
	suite.Require().Equal(&reply.OK{}, re)

	st = suite.lr.MustObjectState(objectRef)
	suite.Require().NotNil(st.ExecutionState)
	suite.Require().Equal(message.NotPending, st.ExecutionState.pending)
	suite.Require().Equal(false, st.ExecutionState.PendingConfirmed)

	// If we already have task in InPending, but it wasn't confirmed
	suite.lr.state[objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour:        &ValidationSaver{},
			Current:          nil,
			Queue:            make([]ExecutionQueueElement, 0),
			pending:          message.InPending,
			PendingConfirmed: false,
		},
	}
	re, err = suite.lr.HandleStillExecutingMessage(suite.ctx, parcel)
	suite.Require().NoError(err)
	suite.Equal(message.InPending, suite.lr.state[objectRef].ExecutionState.pending)
	suite.Equal(true, suite.lr.state[objectRef].ExecutionState.PendingConfirmed)
}

func TestReleaseQueue(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		QueueLength     int
		ExpectedLength  int
		ExpectedHasMore bool
	}{
		"zero":  {0, 0, false},
		"one":   {1, 1, false},
		"max":   {maxQueueLength, maxQueueLength, false},
		"max+1": {maxQueueLength + 1, maxQueueLength, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			a := assert.New(t)

			es := ExecutionState{Queue: make([]ExecutionQueueElement, tc.QueueLength)}
			mq, hasMore := es.releaseQueue()
			a.Equal(tc.ExpectedLength, len(mq))
			a.Equal(tc.ExpectedHasMore, hasMore)
		})
	}
}

func (suite *LogicRunnerTestSuite) TestNoExcessiveAmends() {
	suite.am.UpdateObjectMock.Return(nil, nil)

	randRef := testutils.RandomRef()

	es := &ExecutionState{Queue: make([]ExecutionQueueElement, 0)}
	es.Queue = append(es.Queue, ExecutionQueueElement{})
	es.objectbody = &ObjectBody{}
	es.objectbody.CodeMachineType = core.MachineTypeBuiltin
	es.Current = &CurrentExecution{}
	es.Current.LogicContext = &core.LogicCallContext{}
	es.Current.Request = &randRef
	es.objectbody.CodeRef = &randRef

	data := []byte(testutils.RandomString())
	es.objectbody.Object = data

	mle := testutils.NewMachineLogicExecutorMock(suite.mc)
	suite.lr.Executors[core.MachineTypeBuiltin] = mle
	mle.CallMethodMock.Return(data, nil, nil)

	msg := &message.CallMethod{
		ObjectRef: randRef,
		Method:    "some",
	}

	// In this case Update isn't send to ledger (objects data/newData are the same)
	suite.am.RegisterResultMock.Return(nil, nil)

	_, err := suite.lr.executeMethodCall(suite.ctx, es, msg)
	suite.Require().NoError(err)
	suite.Require().Equal(uint64(0), suite.am.UpdateObjectCounter)

	// In this case Update is send to ledger (objects data/newData are different)
	newData := make([]byte, 5, 5)
	mle.CallMethodMock.Return(newData, nil, nil)

	_, err = suite.lr.executeMethodCall(suite.ctx, es, msg)
	suite.Require().NoError(err)
	suite.Require().Equal(uint64(1), suite.am.UpdateObjectCounter)
}

func (suite *LogicRunnerTestSuite) TestHandleAbandonedRequestsNotificationMessage() {
	objectId := testutils.RandomID()
	msg := &message.AbandonedRequestsNotification{Object: objectId}
	parcel := &message.Parcel{Msg: msg}

	_, err := suite.lr.HandleAbandonedRequestsNotificationMessage(suite.ctx, parcel)
	suite.Require().NoError(err)
	suite.Equal(true, suite.lr.state[*msg.DefaultTarget()].ExecutionState.LedgerHasMoreRequests)

	// LedgerHasMoreRequests false
	suite.lr, _ = NewLogicRunner(&configuration.LogicRunner{})
	suite.lr.state[*msg.DefaultTarget()] = &ObjectState{ExecutionState: &ExecutionState{LedgerHasMoreRequests: false}}

	_, err = suite.lr.HandleAbandonedRequestsNotificationMessage(suite.ctx, parcel)
	suite.Require().NoError(err)
	suite.Equal(true, suite.lr.state[*msg.DefaultTarget()].ExecutionState.LedgerHasMoreRequests)

	// LedgerHasMoreRequests already true
	suite.lr, _ = NewLogicRunner(&configuration.LogicRunner{})
	suite.lr.state[*msg.DefaultTarget()] = &ObjectState{ExecutionState: &ExecutionState{LedgerHasMoreRequests: true}}

	_, err = suite.lr.HandleAbandonedRequestsNotificationMessage(suite.ctx, parcel)
	suite.Require().NoError(err)
	suite.Equal(true, suite.lr.state[*msg.DefaultTarget()].ExecutionState.LedgerHasMoreRequests)
}

func (suite *LogicRunnerTestSuite) TestPrepareObjectStateChangePendingStatus() {
	ref := testutils.RandomRef()

	msg := &message.ExecutorResults{RecordRef: ref}

	// we are in pending and come to ourselves again
	suite.lr.state[ref] = &ObjectState{ExecutionState: &ExecutionState{
		pending: message.InPending, Current: &CurrentExecution{}},
	}
	err := suite.lr.prepareObjectState(suite.ctx, msg)
	suite.Require().NoError(err)
	suite.Equal(message.NotPending, suite.lr.state[ref].ExecutionState.pending)
	suite.Equal(false, suite.lr.state[ref].ExecutionState.PendingConfirmed)

	// previous executor decline pending, trust him
	msg = &message.ExecutorResults{RecordRef: ref, Pending: message.NotPending}
	suite.lr.state[ref] = &ObjectState{ExecutionState: &ExecutionState{
		pending: message.InPending, Current: nil},
	}
	err = suite.lr.prepareObjectState(suite.ctx, msg)
	suite.Require().NoError(err)
	suite.Equal(message.NotPending, suite.lr.state[ref].ExecutionState.pending)
}

func (suite *LogicRunnerTestSuite) TestPrepareObjectStateChangeLedgerHasMoreRequests() {
	ref := testutils.RandomRef()

	msg := &message.ExecutorResults{RecordRef: ref}

	type testCase struct {
		messageStatus             bool
		objectStateStatus         bool
		expectedObjectStateStatue bool
	}

	testCases := []testCase{
		{true, true, true},
		{true, false, true},
		{false, true, true},
		{false, false, false},
	}

	for _, test := range testCases {
		msg = &message.ExecutorResults{RecordRef: ref, LedgerHasMoreRequests: test.messageStatus}
		suite.lr.state[ref] = &ObjectState{ExecutionState: &ExecutionState{QueueProcessorActive: true, LedgerHasMoreRequests: test.objectStateStatus}}
		err := suite.lr.prepareObjectState(suite.ctx, msg)
		suite.Require().NoError(err)
		suite.Equal(test.expectedObjectStateStatue, suite.lr.state[ref].ExecutionState.LedgerHasMoreRequests)
	}
}

func (suite *LogicRunnerTestSuite) TestNewLogicRunner() {
	lr, err := NewLogicRunner(nil)
	suite.Require().Error(err)
	suite.Require().Nil(lr)

	lr, err = NewLogicRunner(&configuration.LogicRunner{})
	suite.Require().NoError(err)
	suite.Require().NotNil(lr)
}

func (suite *LogicRunnerTestSuite) TestStartStop() {
	lr, err := NewLogicRunner(&configuration.LogicRunner{
		BuiltIn: &configuration.BuiltIn{},
	})
	suite.Require().NoError(err)
	suite.Require().NotNil(lr)

	suite.mb.MustRegisterMock.Return()
	lr.MessageBus = suite.mb

	err = lr.Start(suite.ctx)
	suite.Require().NoError(err)

	executor, err := lr.GetExecutor(core.MachineTypeBuiltin)
	suite.NotNil(executor)
	suite.NoError(err)

	err = lr.Stop(suite.ctx)
	suite.Require().NoError(err)
}

func (suite *LogicRunnerTestSuite) TestConcurrency() {
	objectRef := testutils.RandomRef()
	parentRef := testutils.RandomRef()
	protoRef := testutils.RandomRef()
	codeRef := testutils.RandomRef()

	meRef := testutils.RandomRef()
	notMeRef := testutils.RandomRef()
	suite.jc.MeMock.Return(meRef)

	pulse := core.Pulse{PulseNumber: 100}
	suite.ps.CurrentMock.Return(&pulse, nil)

	suite.jc.IsAuthorizedFunc = func(
		ctx context.Context, role core.DynamicRole, id core.RecordID, pn core.PulseNumber, obj core.RecordRef,
	) (bool, error) {
		return true, nil
	}

	mle := testutils.NewMachineLogicExecutorMock(suite.mc)
	err := suite.lr.RegisterExecutor(core.MachineTypeBuiltin, mle)
	suite.Require().NoError(err)

	mle.CallMethodMock.Return([]byte{1, 2, 3}, []byte{}, nil)

	nodeMock := network.NewNodeMock(suite.T())
	nodeMock.IDMock.Return(meRef)
	suite.nn.GetOriginMock.Return(nodeMock)

	od := testutils.NewObjectDescriptorMock(suite.T())
	od.PrototypeMock.Return(&protoRef, nil)
	od.MemoryMock.Return([]byte{1, 2, 3})
	od.ParentMock.Return(&parentRef)
	od.HeadRefMock.Return(&objectRef)

	pd := testutils.NewObjectDescriptorMock(suite.T())
	pd.CodeMock.Return(&codeRef, nil)
	pd.HeadRefMock.Return(&protoRef)

	cd := testutils.NewCodeDescriptorMock(suite.T())
	cd.MachineTypeMock.Return(core.MachineTypeBuiltin)
	cd.RefMock.Return(&codeRef)
	suite.am.GetCodeMock.Return(cd, nil)

	suite.am.GetObjectFunc = func(
		ctx context.Context, obj core.RecordRef, st *core.RecordID, approved bool,
	) (core.ObjectDescriptor, error) {
		switch obj {
		case objectRef:
			return od, nil
		case protoRef:
			return pd, nil
		}
		return nil, errors.New("unexpected call")
	}

	suite.am.GetCodeMock.Return(cd, nil)

	suite.am.HasPendingRequestsMock.Return(false, nil)

	reqId := testutils.RandomID()
	suite.am.RegisterRequestMock.Return(&reqId, nil)
	resId := testutils.RandomID()
	suite.am.RegisterResultMock.Return(&resId, nil)

	num := 100
	wg := sync.WaitGroup{}
	wg.Add(num*2)

	suite.mb.SendFunc = func(
		ctx context.Context, msg core.Message, opts *core.MessageSendOptions,
	) (core.Reply, error) {
		switch msg.Type() {
		case core.TypeReturnResults:
			wg.Done()
			return &reply.OK{}, nil
		}
		suite.Require().Fail(fmt.Sprintf("unexpected message send: %#v", msg))
		return nil, errors.New("unexpected message")
	}

	for i := 0; i < num; i++ {
		go func(i int) {
			msg := &message.CallMethod{
				ObjectRef:      objectRef,
				Method:         "some",
				ProxyPrototype: protoRef,
			}

			parcel := testutils.NewParcelMock(suite.T())
			parcel.DefaultTargetMock.Return(&objectRef)
			parcel.MessageMock.Return(msg)
			parcel.TypeMock.Return(msg.Type())
			parcel.PulseMock.Return(pulse.PulseNumber)
			parcel.GetSenderMock.Return(notMeRef)

			ctx := inslogger.ContextWithTrace(suite.ctx, "req-"+strconv.Itoa(i))

			_, err := suite.lr.Execute(ctx, parcel)
			suite.Require().NoError(err)

			wg.Done()
		}(i)
	}

	wg.Wait()
}

func TestLogicRunner(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(LogicRunnerTestSuite))
}

type LogicRunnerOnPulseTestSuite struct {
	LogicRunnerCommonTestSuite

	pulse     core.Pulse
	objectRef core.RecordRef
}

func (s *LogicRunnerOnPulseTestSuite) BeforeTest(suiteName, testName string) {
	s.LogicRunnerCommonTestSuite.BeforeTest(suiteName, testName)

	s.pulse = core.Pulse{}
	s.objectRef = testutils.RandomRef()
}

func (s *LogicRunnerOnPulseTestSuite) AfterTest(suiteName, testName string) {
	s.LogicRunnerCommonTestSuite.AfterTest(suiteName, testName)
}

// Empty state, expecting no error
func (s *LogicRunnerOnPulseTestSuite) TestEmptyLR() {
	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)
}

// We aren't next executor and we're not executing it
// Expecting empty state of object
func (s *LogicRunnerOnPulseTestSuite) TestEmptyES() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(false, nil)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour: &ValidationSaver{},
		},
	}
	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)
	s.Nil(s.lr.state[s.objectRef])
}

// We aren't next executor and we're not executing it
// Expecting empty execution state
func (s *LogicRunnerOnPulseTestSuite) TestEmptyESWithValidation() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(false, nil)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour: &ValidationSaver{},
		},
		Validation: &ExecutionState{},
		Consensus:  &Consensus{},
	}
	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)
	s.Require().NotNil(s.lr.state[s.objectRef])
	s.Nil(s.lr.state[s.objectRef].ExecutionState)
}

// We aren't next executor but we're currently executing
// Expecting we send message to new executor and moving state to InPending
func (s *LogicRunnerOnPulseTestSuite) TestESWithValidationCurrent() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(false, nil)
	s.mb.SendMock.Return(&reply.ID{}, nil)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour: &ValidationSaver{},
			Current:   &CurrentExecution{},
			pending:   message.NotPending,
		},
	}
	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)
	s.Equal(message.InPending, s.lr.state[s.objectRef].ExecutionState.pending)
}

// We aren't next executor but we're currently executing and queue isn't empty.
// Expecting we send message to new executor and moving state to InPending
func (s *LogicRunnerOnPulseTestSuite) TestWithNotEmptyQueue() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(false, nil)
	s.mb.SendMock.Return(&reply.ID{}, nil)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour: &ValidationSaver{},
			Current:   &CurrentExecution{},
			Queue:     append(make([]ExecutionQueueElement, 0), ExecutionQueueElement{}),
			pending:   message.NotPending,
		},
	}

	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)
	s.Equal(message.InPending, s.lr.state[s.objectRef].ExecutionState.pending)
}

// We aren't next executor but we're currently executing.
// Expecting sending message to new executor and moving state to InPending
func (s *LogicRunnerOnPulseTestSuite) TestWithEmptyQueue() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(false, nil)
	s.mb.SendMock.Return(&reply.ID{}, nil)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour: &ValidationSaver{},
			Current:   &CurrentExecution{},
			Queue:     make([]ExecutionQueueElement, 0),
			pending:   message.NotPending,
		},
	}

	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)
	s.Equal(message.InPending, s.lr.state[s.objectRef].ExecutionState.pending)
}

// Executor is on the same node and we're currently executing
// Expecting task to be moved to NotPending
func (s *LogicRunnerOnPulseTestSuite) TestExecutorSameNode() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(true, nil)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour: &ValidationSaver{},
			Current:   &CurrentExecution{},
			Queue:     make([]ExecutionQueueElement, 0),
			pending:   message.NotPending,
		},
	}

	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)
	s.Require().Equal(message.NotPending, s.lr.state[s.objectRef].ExecutionState.pending)
}

// We're the next executor, task was currently executing and in InPending.
// Expecting task to moved from InPending -> NotPending
func (s *LogicRunnerOnPulseTestSuite) TestStateTransfer1() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(true, nil)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour: &ValidationSaver{},
			Current:   &CurrentExecution{},
			Queue:     make([]ExecutionQueueElement, 0),
			pending:   message.InPending,
		},
	}

	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)
	s.Require().Equal(message.NotPending, s.lr.state[s.objectRef].ExecutionState.pending)
}

// We're the next executor and no one confirmed that this task is executing
// move task from InPending -> NotPending
func (s *LogicRunnerOnPulseTestSuite) TestStateTransfer2() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(true, nil)

	s.am.GetPendingRequestMock.Return(nil, core.ErrNoPendingRequest)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour:        &ValidationSaver{},
			Current:          nil,
			Queue:            make([]ExecutionQueueElement, 0),
			pending:          message.InPending,
			PendingConfirmed: false,
		},
	}

	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)
	s.Require().Equal(message.NotPending, s.lr.state[s.objectRef].ExecutionState.pending)
}

// We're the next executor and previous confirmed that this task is executing
// still in pending
// but we expect that previous executor come to us for token
func (s *LogicRunnerOnPulseTestSuite) TestStateTransfer3() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(true, nil)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour:        &ValidationSaver{},
			Current:          nil,
			Queue:            make([]ExecutionQueueElement, 0),
			pending:          message.InPending,
			PendingConfirmed: true,
		},
	}

	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)

	// we still in pending
	s.Equal(message.InPending, s.lr.state[s.objectRef].ExecutionState.pending)
	// but we expect that previous executor come to us for token
	s.Equal(false, s.lr.state[s.objectRef].ExecutionState.PendingConfirmed)
}

// We're not the next executor, so we must send this task to the next executor
func (s *LogicRunnerOnPulseTestSuite) TestSendTaskToNextExecutor() {
	s.jc.MeMock.Return(core.RecordRef{})
	s.jc.IsAuthorizedMock.Return(false, nil)
	s.mb.SendMock.Return(&reply.ID{}, nil)

	s.lr.state[s.objectRef] = &ObjectState{
		ExecutionState: &ExecutionState{
			Behaviour:        &ValidationSaver{},
			Current:          nil,
			Queue:            make([]ExecutionQueueElement, 0),
			pending:          message.InPending,
			PendingConfirmed: false,
		},
	}

	err := s.lr.OnPulse(s.ctx, s.pulse)
	s.Require().NoError(err)

	_, ok := s.lr.state[s.objectRef]
	s.Equal(false, ok)
}

func (s *LogicRunnerOnPulseTestSuite) TestLedgerHasMoreRequests() {
	s.jc.IsAuthorizedMock.Return(false, nil)
	s.jc.MeMock.Return(core.RecordRef{})

	var testCases = map[string]struct {
		queue           []ExecutionQueueElement
		hasMoreRequests bool
	}{
		"Has": {
			make([]ExecutionQueueElement, maxQueueLength+1),
			true,
		},
		"Don't": {
			make([]ExecutionQueueElement, maxQueueLength),
			false,
		},
	}

	for name, test := range testCases {
		s.T().Run(name, func(t *testing.T) {
			a := assert.New(t)

			messagesQueue := convertQueueToMessageQueue(test.queue[:maxQueueLength])

			expectedMessage := &message.ExecutorResults{
				RecordRef:             s.objectRef,
				Requests:              make([]message.CaseBindRequest, 0),
				Queue:                 messagesQueue,
				LedgerHasMoreRequests: test.hasMoreRequests,
			}

			mb := testutils.NewMessageBusMock(s.mc)
			// defer new SendFunc before calling OnPulse
			mb.SendMock.Set(func(p context.Context, p1 core.Message, p2 *core.MessageSendOptions) (r core.Reply, r1 error) {
				a.Equal(1, int(mb.SendPreCounter))
				a.Equal(expectedMessage, p1)
				return nil, nil
			})

			s.SetupLogicRunner()
			lr := s.lr
			lr.MessageBus = mb
			lr.state[s.objectRef] = &ObjectState{
				ExecutionState: &ExecutionState{
					Behaviour: &ValidationSaver{},
					Queue:     test.queue,
				},
			}

			err := lr.OnPulse(s.ctx, s.pulse)
			a.NoError(err)
		})
	}
}

func TestLogicRunnerOnPulse(t *testing.T) {
	suite.Run(t, new(LogicRunnerOnPulseTestSuite))
}

func TestLRUnsafeGetLedgerPendingRequest(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(LRUnsafeGetLedgerPendingRequestTestSuite))
}

type LRUnsafeGetLedgerPendingRequestTestSuite struct {
	LogicRunnerCommonTestSuite

	pulse                 core.Pulse
	ref                   core.RecordRef
	currentPulseNumber    core.PulseNumber
	oldRequestPulseNumber core.PulseNumber
}

func (s *LRUnsafeGetLedgerPendingRequestTestSuite) BeforeTest(suiteName, testName string) {
	s.LogicRunnerCommonTestSuite.BeforeTest(suiteName, testName)

	s.pulse = core.Pulse{}
	s.ref = testutils.RandomRef()
	s.currentPulseNumber = 3
	s.oldRequestPulseNumber = 1
}

func (s *LRUnsafeGetLedgerPendingRequestTestSuite) AfterTest(suiteName, testName string) {
	s.LogicRunnerCommonTestSuite.AfterTest(suiteName, testName)
}

func (s *LRUnsafeGetLedgerPendingRequestTestSuite) TestAlreadyHaveLedgerQueueElement() {
	es := &ExecutionState{
		Ref:                s.ref,
		Behaviour:          &ValidationSaver{},
		LedgerQueueElement: &ExecutionQueueElement{},
	}

	s.lr.unsafeGetLedgerPendingRequest(s.ctx, es)

	// we check that there is no unexpected calls to A.M., as we already have element
	// from ledger another call to the ledger will return the same request, so we make
	// sure it doesn't happen
}

func (s *LRUnsafeGetLedgerPendingRequestTestSuite) TestNoMoreRequestsInExecutionState() {
	es := &ExecutionState{
		Ref:                   s.ref,
		Behaviour:             &ValidationSaver{},
		LedgerHasMoreRequests: false,
	}
	s.lr.unsafeGetLedgerPendingRequest(s.ctx, es)
	s.Require().Nil(es.LedgerQueueElement)
}

func (s *LRUnsafeGetLedgerPendingRequestTestSuite) TestNoMoreRequestsInLedger() {
	es := &ExecutionState{Ref: s.ref, Behaviour: &ValidationSaver{}, LedgerHasMoreRequests: true}

	am := testutils.NewArtifactManagerMock(s.mc)
	am.GetPendingRequestMock.Return(nil, core.ErrNoPendingRequest)
	s.lr.ArtifactManager = am
	s.lr.unsafeGetLedgerPendingRequest(s.ctx, es)
	s.Equal(false, es.LedgerHasMoreRequests)
}

func (s *LRUnsafeGetLedgerPendingRequestTestSuite) TestDoesNotAuthorized() {
	es := &ExecutionState{Ref: s.ref, Behaviour: &ValidationSaver{}, LedgerHasMoreRequests: true}

	parcel := &message.Parcel{
		PulseNumber: s.oldRequestPulseNumber,
		Msg:         &message.CallMethod{},
	}
	s.am.GetPendingRequestMock.Return(parcel, nil)

	// we doesn't authorized (pulse change in time we process function)
	s.ps.CurrentMock.Return(&core.Pulse{PulseNumber: s.currentPulseNumber}, nil)
	s.jc.IsAuthorizedMock.Return(false, nil)
	s.jc.MeMock.Return(core.RecordRef{})

	s.lr.unsafeGetLedgerPendingRequest(s.ctx, es)
	s.Require().Nil(es.LedgerQueueElement)
}

func (s LRUnsafeGetLedgerPendingRequestTestSuite) TestUnsafeGetLedgerPendingRequest() {
	es := &ExecutionState{Ref: s.ref, Behaviour: &ValidationSaver{}, LedgerHasMoreRequests: true}

	parcel := &message.Parcel{
		PulseNumber: s.oldRequestPulseNumber,
		Msg:         &message.CallMethod{}, // todo add ref
	}
	s.am.GetPendingRequestMock.Return(parcel, nil)

	s.ps.CurrentMock.Return(&core.Pulse{PulseNumber: s.currentPulseNumber}, nil)
	s.jc.IsAuthorizedMock.Return(true, nil)
	s.jc.MeMock.Return(core.RecordRef{})
	s.lr.unsafeGetLedgerPendingRequest(s.ctx, es)

	s.Require().Equal(true, es.LedgerHasMoreRequests)
	s.Require().Equal(parcel, es.LedgerQueueElement.parcel)
}
