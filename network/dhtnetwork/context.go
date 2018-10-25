/*
 *    Copyright 2018 Insolar
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

package dhtnetwork

import (
	"context"

	"github.com/insolar/insolar/network/dhtnetwork/hosthandler"
	"github.com/insolar/insolar/network/transport/id"
	"github.com/pkg/errors"
)

type ctxKey string

const (
	CtxTableIndex = ctxKey("table_index")
	DefaultHostID = 0
)

// ContextBuilder allows to lazy configure and build new Context.
type ContextBuilder struct {
	hostHandler hosthandler.HostHandler
	actions     []func(ctx hosthandler.Context) (hosthandler.Context, error)
}

// NewContextBuilder creates new ContextBuilder.
func NewContextBuilder(hostHandler hosthandler.HostHandler) ContextBuilder {
	return ContextBuilder{
		hostHandler: hostHandler,
	}
}

// Build builds and returns new Context.
func (cb ContextBuilder) Build() (ctx hosthandler.Context, err error) {
	ctx = context.Background()
	for _, action := range cb.actions {
		ctx, err = action(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to build context")
		}
	}
	return
}

// SetHostByID sets host id in Context.
func (cb ContextBuilder) SetHostByID(hostID id.ID) ContextBuilder {
	cb.actions = append(cb.actions, func(ctx hosthandler.Context) (hosthandler.Context, error) {
		for index, id1 := range cb.hostHandler.GetOriginHost().IDs {
			if hostID.Equal(id1.Bytes()) {
				return context.WithValue(ctx, CtxTableIndex, index), nil
			}
		}
		return nil, errors.New("host requestID not found")
	})
	return cb
}

// SetDefaultHost sets first host id in Context.
func (cb ContextBuilder) SetDefaultHost() ContextBuilder {
	cb.actions = append(cb.actions, func(ctx hosthandler.Context) (hosthandler.Context, error) {
		return context.WithValue(ctx, CtxTableIndex, DefaultHostID), nil
	})
	return cb
}