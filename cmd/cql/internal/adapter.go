/*
 * Copyright 2018-2019 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package internal

import (
	"context"
	"flag"
	"time"

	"github.com/CovenantSQL/CovenantSQL/sqlchain/adapter"
	"github.com/CovenantSQL/CovenantSQL/utils"
)

var (
	adapterAddr          string // adapter listen addr
	adapterUseMirrorAddr string
)

// CmdAdapter is cql adapter command entity.
var CmdAdapter = &Command{
	UsageLine: "cql adapter [common params] [-tmp-path path] [-bg-log-level level] [-mirror addr] listen_address",
	Short:     "start a SQLChain adapter server",
	Long: `
Adapter serves a SQLChain adapter.
e.g.
    cql adapter 127.0.0.1:7784
`,
	Flag:       flag.NewFlagSet("Adapter params", flag.ExitOnError),
	CommonFlag: flag.NewFlagSet("Common params", flag.ExitOnError),
	DebugFlag:  flag.NewFlagSet("Debug params", flag.ExitOnError),
}

func init() {
	CmdAdapter.Run = runAdapter
	CmdAdapter.Flag.StringVar(&adapterUseMirrorAddr, "mirror", "", "Mirror server for adapter to query")

	addCommonFlags(CmdAdapter)
	addConfigFlag(CmdAdapter)
	addBgServerFlag(CmdAdapter)
}

func startAdapterServer(adapterAddr string, adapterUseMirrorAddr string) func() {
	adapterHTTPServer, err := adapter.NewHTTPAdapter(adapterAddr, configFile, adapterUseMirrorAddr)
	if err != nil {
		ConsoleLog.WithError(err).Error("init adapter failed")
		SetExitStatus(1)
		return nil
	}

	if err = adapterHTTPServer.Serve(); err != nil {
		ConsoleLog.WithError(err).Error("start adapter failed")
		SetExitStatus(1)
		return nil
	}

	ConsoleLog.Infof("adapter started on %s", adapterAddr)

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		adapterHTTPServer.Shutdown(ctx)
		ConsoleLog.Info("adapter stopped")
	}
}

func runAdapter(cmd *Command, args []string) {
	commonFlagsInit(cmd)

	if len(args) != 1 {
		ConsoleLog.Error("adapter command need listen address as param")
		SetExitStatus(1)
		printCommandHelp(cmd)
		Exit()
	}

	configInit()
	bgServerInit()

	adapterAddr = args[0]

	cancelFunc := startAdapterServer(adapterAddr, adapterUseMirrorAddr)
	ExitIfErrors()
	defer cancelFunc()

	ConsoleLog.Printf("Ctrl + C to stop adapter server on %s\n", adapterAddr)
	<-utils.WaitForExit()
}
