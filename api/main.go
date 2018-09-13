/*
 *    Copyright 2018 INS Ecosystem
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

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/insolar/insolar/configuration"
	"github.com/insolar/insolar/core"
	"github.com/pkg/errors"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func WriteError(message string, code int) map[string]interface{} {
	errJSON := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"code":    code,
		},
	}
	return errJSON
}

func makeHandlerMarshalErrorJSON() []byte {
	jsonErr := WriteError("Invalid data from handler", -1)
	serJSON, err := json.Marshal(jsonErr)
	if err != nil {
		log.Fatal("Can't marshal base error")
	}
	return serJSON
}

var handlerMarshalErrorJSON = makeHandlerMarshalErrorJSON()

func ProcessQueryType(rh *RequestHandler, qTypeStr string) map[string]interface{} {
	qtype := QTypeFromString(qTypeStr)
	var answer map[string]interface{}

	var handlerError error
	switch qtype {
	case CreateMember:
		answer, handlerError = rh.ProcessCreateMember()
	case DumpUserInfo:
		answer, handlerError = rh.ProcessDumpUsers(false)
	case DumpAllUsers:
		answer, handlerError = rh.ProcessDumpUsers(true)
	case GetBalance:
		answer, handlerError = rh.ProcessGetBalance()
	case SendMoney:
		answer, handlerError = rh.ProcessSendMoney()
	default:
		msg := fmt.Sprintf("Wrong query parameter 'query_type' = '%s'", qTypeStr)
		answer = WriteError(msg, -2)
		logrus.Printf("[QID=%s] %s\n", rh.qid, msg)
		return answer
	}
	if handlerError != nil {
		errMsg := "Handler error: " + handlerError.Error()
		log.Printf("[QID=%s] %s\n", rh.qid, errMsg)
		answer = WriteError(errMsg, -3)
	}

	return answer
}

const QIDQueryParam = "qid"

func PreprocessRequest(req *http.Request) (*Params, error) {
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return nil, errors.Wrap(err, "[ PreprocessRequest ] Can't read body. So strange")
	}
	if len(body) == 0 {
		return nil, errors.New("[ PreprocessRequest ] Empty body")
	}

	var params Params
	err = json.Unmarshal(body, &params)
	if err != nil {
		return nil, errors.Wrap(err, "[ PreprocessRequest ] Can't parse input params")
	}

	if len(params.QID) == 0 {
		params.QID = GenQID()
	}

	logrus.Printf("[QID=%s] Query: %s\n", params.QID, string(body))

	return &params, nil
}

func WrapAPIV1Handler(router core.MessageRouter) func(w http.ResponseWriter, r *http.Request) {
	return func(response http.ResponseWriter, req *http.Request) {
		answer := make(map[string]interface{})
		var params *Params
		defer func() {
			if answer == nil {
				answer = make(map[string]interface{})
			}
			if params == nil {
				params = &Params{}
			}
			answer[QIDQueryParam] = params.QID
			serJSON, err := json.MarshalIndent(answer, "", "    ")
			if err != nil {
				serJSON = handlerMarshalErrorJSON
			}
			response.Header().Add("Content-Type", "application/json")
			var newLine byte = '\n'
			_, err = response.Write(append(serJSON, newLine))
			if err != nil {
				logrus.Printf("[QID=%s] Can't write response\n", params.QID)
			}
			logrus.Infof("[QID=%s] Request completed\n", params.QID)
		}()

		params, err := PreprocessRequest(req)
		if err != nil {
			answer = WriteError("Bad request", -3)
			logrus.Errorf("[QID=]Can't parse input request: %s\n", err, req.RequestURI)
			return
		}
		rh := NewRequestHandler(params, router)

		answer = ProcessQueryType(rh, params.QType)
	}
}

type APIRunner struct {
	messageRouter core.MessageRouter
	server        *http.Server
	cfg           *configuration.APIRunner
}

func NewAPIRunner(cfg *configuration.APIRunner) (*APIRunner, error) {
	if cfg == nil {
		return nil, errors.New("[ NewAPIRunner ] config is nil")
	}
	if cfg.Port == 0 {
		return nil, errors.New("[ NewAPIRunner ] Port must not be 0")
	}
	if len(cfg.Location) == 0 {
		return nil, errors.New("[ NewAPIRunner ] Location must exist")
	}

	portStr := fmt.Sprint(cfg.Port)
	ar := APIRunner{
		server: &http.Server{Addr: ":" + portStr},
		cfg:    cfg,
	}

	return &ar, nil
}

func (ar *APIRunner) Start(c core.Components) error {

	// TODO: init message router
	_, ok := c["core.MessageRouter"]
	if !ok {
		logrus.Warnln("Working in demo mode: without MessageRouter")
	} else {
		ar.messageRouter = c["core.MessageRouter"].(core.MessageRouter)
	}

	fw := WrapAPIV1Handler(ar.messageRouter)
	http.HandleFunc(ar.cfg.Location, fw)
	logrus.Info("Starting ApiRunner ...")
	logrus.Info("Config: ", ar.cfg)
	go func() {
		if err := ar.server.ListenAndServe(); err != nil {
			logrus.Errorln("Httpserver: ListenAndServe() error: ", err)
		}
	}()
	return nil
}

func (ar *APIRunner) Stop() error {
	const timeOut = 5
	logrus.Infof("Shutting down server gracefully ...(waiting for %d seconds)", timeOut)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeOut)*time.Second)
	defer cancel()
	err := ar.server.Shutdown(ctx)
	if err != nil {
		return errors.Wrap(err, "Can't gracefully stop API server")
	}

	return nil
}

// func example() {
// 	cfg := configuration.NewAPIRunner()
// 	api, err := NewAPIRunner(&cfg)
// 	if err != nil {
// 		log.Fatal(err)
// 	}
// 	cs := core.Components{}
// 	_ = api.Start(cs)
// 	time.Sleep(60 * time.Second)
// 	_ = api.Stop()
// }
