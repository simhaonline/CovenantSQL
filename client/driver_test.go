/*
 * Copyright 2018 The ThunderDB Authors.
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

package client

import (
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"gitlab.com/thunderdb/ThunderDB/conf"
	"gitlab.com/thunderdb/ThunderDB/consistent"
	"gitlab.com/thunderdb/ThunderDB/crypto/asymmetric"
	"gitlab.com/thunderdb/ThunderDB/crypto/hash"
	"gitlab.com/thunderdb/ThunderDB/crypto/kms"
	"gitlab.com/thunderdb/ThunderDB/kayak"
	"gitlab.com/thunderdb/ThunderDB/pow/cpuminer"
	"gitlab.com/thunderdb/ThunderDB/proto"
	"gitlab.com/thunderdb/ThunderDB/route"
	"gitlab.com/thunderdb/ThunderDB/rpc"
	ct "gitlab.com/thunderdb/ThunderDB/sqlchain/types"
	"gitlab.com/thunderdb/ThunderDB/utils/log"
	"gitlab.com/thunderdb/ThunderDB/worker"
	wt "gitlab.com/thunderdb/ThunderDB/worker/types"
)

var (
	rootHash         = hash.Hash{}
	stopTestService  func()
)

const PubKeyStorePath = "./public.keystore"

// TODO(xq262144), to be replaced with standalone miner binary
func startTestService() (err error) {
	var server *rpc.Server
	var cleanup func()
	if cleanup, server, err = initNode(); err != nil {
		return
	}

	var rootDir string
	if rootDir, err = ioutil.TempDir("", "dbms_test_"); err != nil {
		return
	}

	cfg := &worker.DBMSConfig{
		RootDir:         rootDir,
		Server:          server,
		MaxWriteTimeGap: time.Second * 5,
	}

	var dbms *worker.DBMS
	if dbms, err = worker.NewDBMS(cfg); err != nil {
		return
	}

	stopTestService = func() {
		if dbms != nil {
			dbms.Shutdown()
		}

		cleanup()
	}

	// init
	if err = dbms.Init(); err != nil {
		return
	}

	// add database
	var req *wt.UpdateService
	var res wt.UpdateServiceResponse
	var peers *kayak.Peers
	var block *ct.Block

	dbID := proto.DatabaseID("db")

	// create sqlchain block
	block, err = createRandomBlock(rootHash, true)

	// get database peers
	if peers, err = getPeers(1); err != nil {
		return
	}

	// build create database request
	req = &wt.UpdateService{
		Op: wt.CreateDB,
		Instance: wt.ServiceInstance{
			DatabaseID:   dbID,
			Peers:        peers,
			GenesisBlock: block,
		},
	}

	// send create database request
	if err = testRequest("Update", req, &res); err != nil {
		return
	}

	return
}

func initNode() (cleanupFunc func(), server *rpc.Server, err error) {
	var d string
	if d, err = ioutil.TempDir("", "db_test_"); err != nil {
		return
	}
	log.Debugf("temp dir: %s", d)

	// init conf
	_, testFile, _, _ := runtime.Caller(0)
	pubKeyStoreFile := filepath.Join(d, PubKeyStorePath)
	os.Remove(pubKeyStoreFile)
	confFile := filepath.Join(filepath.Dir(testFile), "../test/node_0/config.yaml")
	privateKeyPath := filepath.Join(filepath.Dir(testFile), "../test/node_0/private.key")

	conf.GConf, _ = conf.LoadConfig(confFile)
	log.Debugf("GConf: %#v", conf.GConf)
	// reset the once
	route.Once = sync.Once{}
	route.InitKMS(pubKeyStoreFile + "_c")

	var dht *route.DHTService

	// init dht
	dht, err = route.NewDHTService(pubKeyStoreFile, new(consistent.KMSStorage), true)
	if err != nil {
		return
	}

	// init rpc
	if server, err = rpc.NewServerWithService(rpc.ServiceMap{"DHT": dht}); err != nil {
		return
	}

	// init private key
	masterKey := []byte("")
	addr := "127.0.0.1:0"
	server.InitRPCServer(addr, privateKeyPath, masterKey)

	// start server
	go server.Serve()

	// fixme: force set the bp addr to this server
	route.SetNodeAddrCache(&conf.GConf.BP.RawNodeID, server.Listener.Addr().String())

	cleanupFunc = func() {
		os.RemoveAll(d)
		server.Listener.Close()
		server.Stop()
	}

	return
}

// copied from sqlchain.xxx_test.
func createRandomBlock(parent hash.Hash, isGenesis bool) (b *ct.Block, err error) {
	// Generate key pair
	priv, pub, err := asymmetric.GenSecp256k1KeyPair()

	if err != nil {
		return
	}

	h := hash.Hash{}
	rand.Read(h[:])

	b = &ct.Block{
		SignedHeader: ct.SignedHeader{
			Header: ct.Header{
				Version:     0x01000000,
				Producer:    proto.NodeID(h.String()),
				GenesisHash: rootHash,
				ParentHash:  parent,
				Timestamp:   time.Now().UTC(),
			},
			Signee:    pub,
			Signature: nil,
		},
		Queries: make([]*hash.Hash, rand.Intn(10)+10),
	}

	for i := range b.Queries {
		b.Queries[i] = new(hash.Hash)
		rand.Read(b.Queries[i][:])
	}

	if isGenesis {
		// Compute nonce with public key
		nonceCh := make(chan cpuminer.NonceInfo)
		quitCh := make(chan struct{})
		miner := cpuminer.NewCPUMiner(quitCh)
		go miner.ComputeBlockNonce(cpuminer.MiningBlock{
			Data:      pub.Serialize(),
			NonceChan: nonceCh,
			Stop:      nil,
		}, cpuminer.Uint256{A: 0, B: 0, C: 0, D: 0}, 4)
		nonce := <-nonceCh
		close(quitCh)
		close(nonceCh)
		// Add public key to KMS
		id := cpuminer.HashBlock(pub.Serialize(), nonce.Nonce)
		b.SignedHeader.Header.Producer = proto.NodeID(id.String())
		err = kms.SetPublicKey(proto.NodeID(id.String()), nonce.Nonce, pub)

		if err != nil {
			return nil, err
		}
	}

	err = b.PackAndSignBlock(priv)
	return
}

func testRequest(method string, req interface{}, response interface{}) (err error) {
	realMethod := "DBS." + method

	// get node id
	var nodeID proto.NodeID
	if nodeID, err = kms.GetLocalNodeID(); err != nil {
		return
	}

	var conn net.Conn
	if conn, err = rpc.DialToNode(nodeID, nil); err != nil {
		return
	}

	var client *rpc.Client
	if client, err = rpc.InitClientConn(conn); err != nil {
		conn.Close()
		return
	}

	defer client.Close()

	return client.Call(realMethod, req, response)
}

func getKeys() (privKey *asymmetric.PrivateKey, pubKey *asymmetric.PublicKey, err error) {
	// get public key
	if pubKey, err = kms.GetLocalPublicKey(); err != nil {
		return
	}

	// get private key
	if privKey, err = kms.GetLocalPrivateKey(); err != nil {
		return
	}

	return
}

func getPeers(term uint64) (peers *kayak.Peers, err error) {
	// get node id
	var nodeID proto.NodeID
	if nodeID, err = kms.GetLocalNodeID(); err != nil {
		return
	}

	// get private/public key
	var pubKey *asymmetric.PublicKey
	var privateKey *asymmetric.PrivateKey

	if privateKey, pubKey, err = getKeys(); err != nil {
		return
	}

	// generate peers and sign
	server := &kayak.Server{
		Role:   conf.Leader,
		ID:     nodeID,
		PubKey: pubKey,
	}
	peers = &kayak.Peers{
		Term:    term,
		Leader:  server,
		Servers: []*kayak.Server{server},
		PubKey:  pubKey,
	}
	err = peers.Sign(privateKey)
	return
}
