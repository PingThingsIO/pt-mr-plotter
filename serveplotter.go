/*
 * Copyright (C) 2016 Sam Kumar, Michael Andersen, and the University
 * of California, Berkeley.
 *
 * This file is part of Mr. Plotter (the Multi-Resolution Plotter).
 *
 * Mr. Plotter is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published
 * by the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Mr. Plotter is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Mr. Plotter.  If not, see <http://www.gnu.org/licenses/>.
 */

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/ini.v1"

	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"

	httpHandlers "github.com/gorilla/handlers"
	ws "github.com/gorilla/websocket"
	uuid "github.com/pborman/uuid"
)

const (
	FORWARD_CHUNKSIZE   int    = (4 << 10)  // 4 KiB
	MAX_REQSIZE         int64  = (16 << 10) // 16 KiB
	ERROR_INVALID_TOKEN string = "Invalid token"

	MONGO_ID_LEN int = 12
)

type CSVRequest struct {
	StartTime   int64
	EndTime     int64
	UUIDs       []string `json:"UUIDS"`
	Labels      []string
	UnitofTime  string
	Token       string `json:"_token,omitempty"`
	WindowWidth int64
}

var upgrader = ws.Upgrader{}

type RespWrapper struct {
	wr io.Writer
}

func (rw RespWrapper) GetWriter() io.Writer {
	return rw.wr
}

/* State needed to handle HTTP requests. */
var btrdbConn *btrdb.BTrDB
var dr *DataRequester
var br *DataRequester
var etcdConn *etcd.Client
var permalinklen int
var csvMaxPoints uint64
var dataTimeout time.Duration
var bracketTimeout time.Duration
var csvTimeout time.Duration
var mdTimeout time.Duration
var permalinkNumBytes int
var permalinkMaxTries int

/* I don't order these elements from largest to smallest, so the int64s at the
   bottom may not be 8-byte aligned. That's OK, because I don't anticipate
   doing any atomic operations on these, and regular operations don't have to
   be particularly fast (I'm just parsing a config file, after all). */
type Config struct {
	HttpPort      uint16
	HttpsPort     uint16
	UseHttp       bool
	UseHttps      bool
	HttpsRedirect bool
	PlotterDir    string
	CertFile      string
	KeyFile       string

	DbAddr             string
	NumDataConn        uint16
	NumBracketConn     uint16
	MaxDataRequests    uint32
	MaxBracketRequests uint32
	MetadataServer     string
	MongoServer        string
	CsvUrl             string

	SessionExpirySeconds          int64
	SessionPurgeIntervalSeconds   int64
	CsvMaxPointsPerStream         int64
	OutstandingRequestLogInterval int64
	NumGoroutinesLogInterval      int64
	DbDataTimeoutSeconds          int64
	DbBracketTimeoutSeconds       int64
}

var configRequiredKeys = map[string]bool{
	"http_port":      true,
	"https_port":     true,
	"use_http":       true,
	"use_https":      true,
	"https_redirect": true,
	"plotter_dir":    true,
	"cert_file":      true,
	"key_file":       true,

	"db_addr":              true,
	"num_data_conn":        true,
	"num_bracket_conn":     true,
	"max_data_requests":    true,
	"max_bracket_requests": true,
	"metadata_server":      true,
	"mongo_server":         true,
	"csv_url":              true,

	"session_expiry_seconds":           true,
	"session_purge_interval_seconds":   true,
	"csv_max_points_per_stream":        true,
	"outstanding_request_log_interval": true,
	"num_goroutines_log_interval":      true,
	"db_data_timeout_seconds":          true,
	"db_bracket_timeout_seconds":       true,
}

func main() {
	var config Config
	var err error
	var filename string

	if len(os.Args) < 2 {
		filename = "plotter.ini"
	} else {
		filename = os.Args[1]
	}

	rawConfig, err := ini.Load(filename)
	if err != nil {
		log.Fatalf("Could not parse %s: %v", filename, err)
	}

	/* Validate the configuration file. */
	defaultSect := rawConfig.Section("")
	for requiredKey, required := range configRequiredKeys {
		if required && !defaultSect.HasKey(requiredKey) {
			log.Fatalf("Configuration file is missing required key \"%s\"", requiredKey)
		}
	}

	rawConfig.NameMapper = ini.TitleUnderscore
	err = rawConfig.MapTo(&config)
	if err != nil {
		log.Fatalf("Could not map configuration file: %v", err)
	}

	if len(config.BtrdbEndpoints) == 0 {
		config.BtrdbEndpoints = btrdb.EndpointsFromEnv()
	}

	if config.UseHttps {
		start := make(chan struct{}, 1)
		go func() {
			httpsCertPrefix := accounts.GetTagEtcdPath()
			watchchan := etcdConn.Watch(context.Background(), httpsCertPrefix)
			<-start
			for watchresp := range watchchan {
				err2 := watchresp.Err()
				if err2 != nil {
					log.Fatalf("Error watching https certificates: %v", err2)
				}
				updateTLSConfig(&config)
			}

			log.Fatalln("Watch on tags was lost")
		}()
		updateTLSConfig(&config)
		start <- struct{}{}
	}

	var sessionkeys *keys.SessionKeys
	sessionkeys, err = keys.RetrieveSessionKeys(context.Background(), etcdConn)
	if err != nil {
		log.Fatalf("Could not get session keys from etcd: %v", err)
	}
	var sessionencryptkey []byte
	var sessionmackey []byte

	if sessionkeys != nil {
		log.Println("Found session keys in etcd")
		sessionencryptkey = sessionkeys.EncryptKey
		sessionmackey = sessionkeys.MACKey
	} else {
		log.Println("Session keys not in etcd; generating session keys...")
		var keybytes = make([]byte, 32)
		if _, err = rand.Read(keybytes); err != nil {
			log.Fatalf("Could not generate session keys: %v", err)
		}
		sessionkeys = &keys.SessionKeys{
			EncryptKey: keybytes[:16],
			MACKey:     keybytes[16:],
		}
		_, err = keys.UpsertSessionKeysAtomically(context.Background(), etcdConn, sessionkeys)
		if err != nil {
			log.Fatalf("Could not update session key: %v", err)
		}
		/* Now read the keys from etcd. Don't just use the bytes in case the
		   atomic update failed. */
		sessionkeys, err = keys.RetrieveSessionKeys(context.Background(), etcdConn)
		if err != nil {
			log.Fatalf("Could not get session keys from etcd after insert: %v", err)
		}
		sessionencryptkey = sessionkeys.EncryptKey
		sessionmackey = sessionkeys.MACKey
	}

	if bytes.Equal(sessionencryptkey, sessionmackey) {
		log.Fatalln("The session encryption and MAC keys are the same; to ensure that session state is stored securely on the client, please change them to be different")
	}

	err = setEncryptKey(sessionencryptkey)
	if err != nil {
		log.Fatalf("Invalid encryption key: %v", err)
	}
	err = setMACKey(sessionmackey)
	if err != nil {
		log.Fatalf("Invalid MAC key: %v", err)
	}

	setTagPermissionCacheSize(config.MaxCachedTagPermissions)

	csvMaxPoints = config.CsvMaxPointsPerStream

	mongoConn, err := mgo.Dial(config.MongoServer)
	if err != nil {
		log.Fatalf("Error: %v\n", err)
		os.Exit(1)
	}

	plotterDBConn := mongoConn.DB("mr_plotter")
	permalinkConn = plotterDBConn.C("permalinks")
	accountConn = plotterDBConn.C("accounts")

	dr = NewDataRequester(config.DbAddr, int(config.NumDataConn), config.MaxDataRequests, time.Duration(config.DbDataTimeoutSeconds)*time.Second, false)
	if dr == nil {
		os.Exit(1)
	}
	br = NewDataRequester(config.DbAddr, int(config.NumBracketConn), config.MaxBracketRequests, time.Duration(config.DbBracketTimeoutSeconds)*time.Second, true)
	if br == nil {
		os.Exit(1)
	}

	go purgeSessionsPeriodically(config.SessionExpirySeconds, config.SessionPurgeIntervalSeconds)

	go logWaitingRequests(os.Stdout, time.Duration(config.OutstandingRequestLogInterval)*time.Second)

	go logNumGoroutines(os.Stdout, time.Duration(config.NumGoroutinesLogInterval)*time.Second)

	token64len = base64.StdEncoding.EncodedLen(TOKEN_BYTE_LEN)
	token64dlen = base64.StdEncoding.DecodedLen(token64len)
	permalinklen = base64.URLEncoding.EncodedLen(MONGO_ID_LEN)
	permalinkdlen = base64.URLEncoding.DecodedLen(permalinklen)

	http.Handle("/", http.FileServer(http.Dir(config.PlotterDir)))
	http.HandleFunc("/dataws", datawsHandler)
	http.HandleFunc("/data", dataHandler)
	http.HandleFunc("/bracketws", bracketwsHandler)
	http.HandleFunc("/bracket", bracketHandler)
	http.HandleFunc("/treetop", treetopHandler)
	http.HandleFunc("/treebranch", treebranchHandler)
	http.HandleFunc("/treeleaf", treeleafHandler)
	http.HandleFunc("/metadataleaf", metadataleafHandler)
	http.HandleFunc("/metadatauuid", metadatauuidHandler)
	http.HandleFunc("/permalink", permalinkHandler)
	http.HandleFunc("/csv", csvHandler)
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/logoff", logoffHandler)
	http.HandleFunc("/changepw", changepwHandler)
	http.HandleFunc("/checktoken", checktokenHandler)

	var loggedHandler http.Handler = httpHandlers.CompressHandler(httpHandlers.CombinedLoggingHandler(os.Stdout, http.DefaultServeMux))

	var portStrHTTP string = fmt.Sprintf(":%d", config.HttpPort)
	var portStrHTTPS string = fmt.Sprintf(":%d", config.HttpsPort)
	if config.UseHttp && config.UseHttps {
		go func() {
			log.Fatal(http.ListenAndServeTLS(portStrHTTPS, config.CertFile, config.KeyFile, loggedHandler))
			os.Exit(1)
		}()

		if config.HttpsRedirect {
			var redirect http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var url *url.URL = r.URL
				url.Scheme = "https"
				url.Host = r.Host + portStrHTTPS
				http.Redirect(w, r, url.String(), http.StatusFound)
			})
			var loggedRedirect http.Handler = httpHandlers.CompressHandler(httpHandlers.CombinedLoggingHandler(os.Stdout, redirect))
			log.Fatal(http.ListenAndServe(portStrHTTP, loggedRedirect))
		} else {
			log.Fatal(http.ListenAndServe(portStrHTTP, mrPlotterHandler))
		}
	} else if config.UseHttps {
		log.Fatal(mrPlotterServer.ListenAndServeTLS("", ""))
	} else if config.UseHttp {
		log.Fatal(mrPlotterServer.ListenAndServe())
	}
	os.Exit(1)
}

func logWaitingRequests(period time.Duration) {
	for {
		time.Sleep(period)
		log.Printf("Waiting data requests: %v; Waiting bracket requests: %v", dr.totalWaiting, br.totalWaiting)
	}
}

func logNumGoroutines(period time.Duration) {
	for {
		time.Sleep(period)
		log.Printf("Number of goroutines: %v", runtime.NumGoroutine())
	}
}

func parseDataRequest(request string, writ Writable) (uuidBytes uuid.UUID, startTime int64, endTime int64, pw uint8, extra1 string, extra2 string, success bool) {
	var args []string = strings.Split(string(request), ",")
	var err error

	success = false
	var w io.Writer

	if len(args) != 4 && len(args) != 5 && len(args) != 6 {
		w = writ.GetWriter()
		w.Write([]byte(fmt.Sprintf("Four, five, or six arguments are required; got %v", len(args))))
		return
	}

	if len(args) == 6 {
		extra1 = args[4]
		extra2 = args[5]
	} else if len(args) == 5 {
		extra1 = args[4]
	}

	uuidBytes = uuid.Parse(args[0])

	if uuidBytes == nil {
		w = writ.GetWriter()
		w.Write([]byte(fmt.Sprintf("Invalid UUID: got %v", args[0])))
		return
	}
	var pwTemp int64

	startTime, err = strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		w = writ.GetWriter()
		w.Write([]byte(fmt.Sprintf("Could not interpret %v as an int64: %v", args[1], err)))
		return
	}

	endTime, err = strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		w = writ.GetWriter()
		w.Write([]byte(fmt.Sprintf("Could not interpret %v as an int64: %v", args[2], err)))
		return
	}

	pwTemp, err = strconv.ParseInt(args[3], 10, 16)
	if err != nil {
		w = writ.GetWriter()
		w.Write([]byte(fmt.Sprintf("Could not interpret %v as an int16: %v", args[3], err)))
		return
	}

	pw = uint8(pwTemp)

	startTime = ((startTime >> pw) << pw)
	endTime = (((endTime >> pw) + 1) << pw) // we add one pointwidth to the endtime to simulate an inclusive endpoint

	success = true

	return
}

func parseBracketRequest(request string, writ Writable, expectExtra bool) (uuids []uuid.UUID, token string, extra string, success bool) {
	var args []string = strings.Split(string(request), ",")

	success = false
	var w io.Writer

	var numUUIDs int

	if expectExtra {
		numUUIDs = len(args) - 2
	} else {
		numUUIDs = len(args) - 1
	}

	if numUUIDs < 1 {
		w = writ.GetWriter()
		w.Write([]byte(fmt.Sprintf("Got only %v arguments", len(args))))
		return
	}

	if expectExtra {
		extra = args[numUUIDs+1]
	}

	uuids = make([]uuid.UUID, numUUIDs)

	for i := 0; i < numUUIDs; i++ {
		uuids[i] = uuid.Parse(args[i])
		if uuids[i] == nil {
			w = writ.GetWriter()
			w.Write([]byte(fmt.Sprintf("Received invalid UUID %v", args[i])))
			return
		}
	}

	token = args[numUUIDs]

	success = true

	return
}

func validateToken(token string) *LoginSession {
	tokenslice, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil
	}
	return getloginsession(tokenslice)
}

func datawsHandler(w http.ResponseWriter, r *http.Request) {
	var websocket *ws.Conn
	var upgradeerr error
	websocket, upgradeerr = upgrader.Upgrade(w, r, nil)
	if upgradeerr != nil {
		w.Write([]byte(fmt.Sprintf("Could not upgrade HTTP connection to WebSocket: %v\n", upgradeerr)))
		return
	}

	cw := ConnWrapper{
		Writing: &sync.Mutex{},
		Conn:    websocket,
	}

	websocket.SetReadLimit(MAX_REQSIZE)

	for {
		_, payload, err := websocket.ReadMessage()

		if err != nil {
			return // Most likely the connection was closed or the message was too big
		}

		uuidBytes, startTime, endTime, pw, token, echoTag, success := parseDataRequest(string(payload), &cw)

		if success {
			var loginsession *LoginSession
			if token != "" {
				loginsession = validateToken(token)
				if loginsession == nil {
					w.Write([]byte(ERROR_INVALID_TOKEN))
					return
				}
			}
			var ctx = r.Context()
			var cancelfunc context.CancelFunc
			if dataTimeout >= 0 {
				ctx, cancelfunc = context.WithTimeout(ctx, dataTimeout)
			} else {
				ctx, cancelfunc = context.WithCancel(ctx)
			}
			if hasPermission(ctx, loginsession, uuidBytes) {
				dr.MakeDataRequest(ctx, uuidBytes, startTime, endTime, uint8(pw), &cw)
				cancelfunc()
			} else {
				cancelfunc()
				cw.GetWriter().Write([]byte("[]"))
			}
		}
		if cw.CurrWriter != nil {
			cw.CurrWriter.Close()
		}

		writer, err := websocket.NextWriter(ws.TextMessage)
		if err != nil {
			log.Printf("Could not echo tag to client: %v", err)
		}

		if cw.CurrWriter != nil {
			_, err = writer.Write([]byte(echoTag))
			if err != nil {
				log.Printf("Could not echo tag to client: %v", err)
			}
			writer.Close()
		}

		cw.Writing.Unlock()
	}
}

func dataHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("You must send a POST request to get data."))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MAX_REQSIZE)
	payload, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.Write([]byte(fmt.Sprintf("Could not read received POST payload: %v", err)))
		return
	}

	var wrapper RespWrapper = RespWrapper{w}

	uuidBytes, startTime, endTime, pw, token, _, success := parseDataRequest(string(payload), wrapper)

	if success {
		var loginsession *LoginSession
		if token != "" {
			loginsession = validateToken(token)
			if loginsession == nil {
				w.Write([]byte(ERROR_INVALID_TOKEN))
				return
			}
		}
		var ctx = r.Context()
		var cancelfunc context.CancelFunc
		if dataTimeout >= 0 {
			ctx, cancelfunc = context.WithTimeout(ctx, dataTimeout)
		} else {
			ctx, cancelfunc = context.WithCancel(ctx)
		}
		if hasPermission(ctx, loginsession, uuidBytes) {
			dr.MakeDataRequest(ctx, uuidBytes, startTime, endTime, uint8(pw), wrapper)
			cancelfunc()
		} else {
			cancelfunc()
			wrapper.GetWriter().Write([]byte("[]"))
		}
	}
}

func bracketwsHandler(w http.ResponseWriter, r *http.Request) {
	var websocket *ws.Conn
	var upgradeerr error
	websocket, upgradeerr = upgrader.Upgrade(w, r, nil)
	if upgradeerr != nil {
		w.Write([]byte(fmt.Sprintf("Could not upgrade HTTP connection to WebSocket: %v\n", upgradeerr)))
		return
	}

	cw := ConnWrapper{
		Writing: &sync.Mutex{},
		Conn:    websocket,
	}

	websocket.SetReadLimit(MAX_REQSIZE)

	for {
		_, payload, err := websocket.ReadMessage()

		if err != nil {
			return // Most likely the connection was closed or the message was too big
		}

		uuids, token, echoTag, success := parseBracketRequest(string(payload), &cw, true)

		if success {
			var loginsession *LoginSession
			if token != "" {
				loginsession = validateToken(token)
				if loginsession == nil {
					w.Write([]byte(ERROR_INVALID_TOKEN))
					return
				}
			}
			var viewable []uuid.UUID = uuids[:0]
			var ctx = r.Context()
			var cancelfunc context.CancelFunc
			if bracketTimeout >= 0 {
				ctx, cancelfunc = context.WithTimeout(ctx, bracketTimeout)
			} else {
				ctx, cancelfunc = context.WithCancel(ctx)
			}
			for _, uuid := range uuids {
				if hasPermission(ctx, loginsession, uuid) {
					viewable = append(viewable, uuid)
				}
			}
			br.MakeBracketRequest(ctx, uuids, &cw)
			cancelfunc()
		}
		if cw.CurrWriter != nil {
			cw.CurrWriter.Close()
		}

		writer, err := websocket.NextWriter(ws.TextMessage)
		if err != nil {
			log.Printf("Could not echo tag to client: %v", err)
		}

		if cw.CurrWriter != nil {
			_, err = writer.Write([]byte(echoTag))
			if err != nil {
				log.Printf("Could not echo tag to client: %v", err)
			}
			writer.Close()
		}

		cw.Writing.Unlock()
	}
}

func bracketHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("You must send a POST request to get data."))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MAX_REQSIZE)
	payload, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.Write([]byte(fmt.Sprintf("Could not read received POST payload: %v", err)))
		return
	}

	wrapper := RespWrapper{w}

	uuids, token, _, success := parseBracketRequest(string(payload), wrapper, false)

	if success {
		var loginsession *LoginSession
		if token != "" {
			loginsession = validateToken(token)
			if loginsession == nil {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(ERROR_INVALID_TOKEN))
				return
			}
		}
		var ctx = r.Context()
		var cancelfunc context.CancelFunc
		if bracketTimeout >= 0 {
			ctx, cancelfunc = context.WithTimeout(ctx, bracketTimeout)
		} else {
			ctx, cancelfunc = context.WithCancel(ctx)
		}

		filtereduuids := uuids[:0]
		for _, uuid := range uuids {
			if hasPermission(ctx, loginsession, uuid) {
				filtereduuids = append(filtereduuids, uuid)
			}
		}
		br.MakeBracketRequest(ctx, filtereduuids, wrapper)
		cancelfunc()
	}
}

func metadataHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("You must send a POST request to get data."))
		return true
	}

	var n int

	r.Body = http.MaxBytesReader(w, r.Body, MAX_REQSIZE)
	request, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.Write([]byte(fmt.Sprintf("Could not read received POST payload: %v", err)))
		return nil, false
	}
	return request, true
}

func treetopHandler(w http.ResponseWriter, r *http.Request) {
	if onlyallowpost(w, r) {
		return
	}

	var tags string = "public"
	semicolonindex := bytes.IndexByte(request, ';')
	if semicolonindex != -1 {
		tokenencoded := request[semicolonindex+1:]
		request = request[:semicolonindex+1]

		if len(tokenencoded) == token64len {
			tokenslice := make([]byte, token64dlen, token64dlen)
			n, err = base64.StdEncoding.Decode(tokenslice, tokenencoded)
			if n == TOKEN_BYTE_LEN && err == nil {
				tagslice := usertags(tokenslice)
				if tagslice != nil {
					tags = strings.Join(tagslice, ",")
				}
			}
		}
	}

	mdReq, err := http.NewRequest("POST", fmt.Sprintf("%s?tags=%s", mdServer, tags), strings.NewReader(string(request)))
	if err != nil {
		w.Write([]byte(fmt.Sprintf("Error: %v\n", err)))
		return
	}

	mdReq.Header.Set("Content-Type", "text")
	mdReq.Header.Set("Content-Length", fmt.Sprintf("%v", len(request)))
	resp, err := http.DefaultClient.Do(mdReq)

	if err != nil {
		w.Write([]byte(fmt.Sprintf("Error: %v\n", err)))
	}
}

func mdDispatch(w http.ResponseWriter, r *http.Request, dispatch func(context.Context, *etcd.Client, *btrdb.BTrDB, *LoginSession, string) ([]byte, error)) {
	if onlyallowpost(w, r) {
		return
	}

	var buffer []byte = make([]byte, FORWARD_CHUNKSIZE) // forward the response

	var bytesRead int
	var readErr error = nil
	for readErr == nil {
		bytesRead, readErr = resp.Body.Read(buffer)
		w.Write(buffer[:bytesRead])
	}
	resp.Body.Close()
	}
	w.Write(toplevel)
}

func treebranchHandler(w http.ResponseWriter, r *http.Request) {
	mdDispatch(w, r, func(ctx context.Context, ec *etcd.Client, bc *btrdb.BTrDB, ls *LoginSession, toplevel string) ([]byte, error) {
		levels, err := treebranchPaths(ctx, ec, bc, ls, toplevel)
		if err != nil {
			return nil, err
		}
		return json.Marshal(levels)
	})
}

func treeleafHandler(w http.ResponseWriter, r *http.Request) {
	mdDispatch(w, r, func(ctx context.Context, ec *etcd.Client, bc *btrdb.BTrDB, ls *LoginSession, branch string) ([]byte, error) {
		levels, err := treeleafPaths(ctx, ec, bc, ls, branch)
		if err != nil {
			return nil, err
		}
		return json.Marshal(levels)
	})
}

const PERMALINK_HELP string = "To create a permalink, send the data as a JSON document via a POST request. To retrieve a permalink, set a GET request, specifying \"id=<permalink identifier>\" in the URL."
const PERMALINK_BAD_ID string = "not found"

func permalinkHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		w.Header().Set("Allow", "GET POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte(PERMALINK_HELP))
		return
	}

	var err error
	var jsonPermalink map[string]interface{}
	var id bson.ObjectId

	r.Body = http.MaxBytesReader(w, r.Body, MAX_REQSIZE)
	if r.Method == "GET" {
		r.ParseForm()
		var id64str string = r.Form.Get("id")
		if id64str == "" {
			w.Write([]byte(PERMALINK_HELP))
			return
		}

		/* For backwards-compatibility with permalinks from the Meteor plotter, only look at the first 16 bytes. */
		var idslice []byte = make([]byte, permalinkdlen)
		_, err = base64.URLEncoding.Decode(idslice, []byte(id64str)[:permalinklen])

		if err != nil {
			w.Write([]byte(PERMALINK_HELP))
			return
		}

		id = bson.ObjectId(idslice)

		if !id.Valid() {
			w.Write([]byte(PERMALINK_BAD_ID))
			return
		}

		var query *mgo.Query = permalinkConn.FindId(id)

		err = query.One(&jsonPermalink)
		if err != nil {
			w.Write([]byte(PERMALINK_BAD_ID))
			return
		}

		// I could do this asynchronously, but I think this is good enough
		err = permalinkConn.UpdateId(id, map[string]interface{}{
			"$set": map[string]interface{}{
				"lastAccessed": bson.Now(),
			},
		})

		if err != nil {
			// In the future I could try something like restarting the connection
			fmt.Printf("Could not update permalink record: %v\n", err)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		var permalinkEncoder *json.Encoder = json.NewEncoder(w)
		err = permalinkEncoder.Encode(jsonPermalink)

		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("Could not read received POST payload: %v", err)))
			return
		}
	} else {
		var permalinkDecoder *json.Decoder = json.NewDecoder(r.Body)

		err = permalinkDecoder.Decode(&jsonPermalink)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(fmt.Sprintf("Received invalid JSON: %v", err)))
			return
		}

		err = validatePermalinkJSON(jsonPermalink)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		id = bson.NewObjectId()
		jsonPermalink["_id"] = id
		jsonPermalink["lastAccessed"] = bson.Now()

		err = permalinkConn.Insert(jsonPermalink)

		if err == nil {
			id64len := base64.URLEncoding.EncodedLen(len(id))
			id64buf := make([]byte, id64len, id64len)
			base64.URLEncoding.Encode(id64buf, []byte(id))

			success, err = permalink.InsertPermalinkData(ctx, etcdConn, string(id64buf), jsonLiteral)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(fmt.Sprintf("Could not insert permalink into database: %v", err)))
				return
			}
		}

		if success {
			w.Write(id64buf)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Server error"))

			log.Printf("Could not find a unique permalink ID in %d tries!", permalinkMaxTries)
		}
	}
}

// RawCSVRequest encapsulates a request to the Mr. Plotter backend for a CSV.
type RawCSVRequest struct {
	StartTime  int64
	EndTime    int64
	UUIDs      []string `json:"UUIDS"`
	Labels     []string
	QueryType  string
	WindowText string
	WindowUnit string
	UnitofTime string
	Token      string `json:"_token,omitempty"`
	PointWidth uint8
}

func csvHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, "To get a CSV file, send the required data as a JSON document via a POST request.")
		return
	}

	var err error

	r.Body = http.MaxBytesReader(w, r.Body, MAX_REQSIZE)
	_, err = io.ReadFull(r.Body, make([]byte, 5)) // Remove the "json="
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "Bad request")
		return
	}

	var jsonCSVReq CSVRequest
	var jsonCSVReqDecoder *json.Decoder = json.NewDecoder(r.Body)
	err = jsonCSVReqDecoder.Decode(&jsonCSVReq)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "Malformed request")
		return
	}

	if jsonCSVReq.WindowWidth < 0 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("Invalid window width: %d", jsonCSVReq.WindowWidth)))
		return
	}

	/* Check the number of points per stream to see if this request is reasonable. */
	var deltaT int64 = jsonCSVReq.EndTime - jsonCSVReq.StartTime

	/* Taken from the BTrDB HTTP interface bindings, to make sure I handle the units in the same way. */
	switch jsonCSVReq.UnitofTime {
	case "s":
		cq.StartTime *= 1000000000
		cq.EndTime *= 1000000000
	case "":
		fallthrough
	case "ms":
		cq.StartTime *= 1000000
		cq.EndTime *= 1000000
	case "us":
		cq.StartTime *= 1000
		cq.EndTime *= 1000
	case "ns":
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("Invalid unit of time: must be 'ns', 'ms', 'us' or 's' (got '%s')", jsonCSVReq.UnitofTime)))
		return
	}

	var pps int64 = deltaT / jsonCSVReq.WindowWidth
	if pps > csvMaxPoints {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("CSV file too big: estimated %d points", pps)))
		return
	}

	var loginsession *LoginSession
	if jsonCSVReq.Token != "" {
		loginsession = validateToken(jsonCSVReq.Token)
		if loginsession == nil {
			w.WriteHeader(http.StatusBadRequest)
			// Don't use ERROR_INVALID_TOKEN since this opens on a new page, not in the plotting application
			w.Write([]byte("Session expired"))
			return
		}
	}

	for _, uuidstr := range jsonCSVReq.UUIDs {
		uuidobj := uuid.Parse(uuidstr)
		if uuidobj == nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "Malformed UUID")
			return
		}

		if !hasPermission(ctx, loginsession, uuidobj) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "Insufficient permissions")
			return
		}

		s := btrdbConn.StreamFromUUID(uuidobj)
		ex, err2 := s.Exists(ctx)
		if err2 != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "%s", err2.Error())
			return
		}
		if !ex {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "Stream does not exist")
			return
		}

		cq.Streams = append(cq.Streams, s)
	}

	// Don't send the token to BTrDB
	jsonCSVReq.Token = ""

	w.Header().Set("Content-Disposition", "attachment; filename=data.csv")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Transfer-Encoding", "chunked")

	var csvJSON []byte
	csvJSON, err = json.Marshal(&jsonCSVReq)
	fmt.Printf("forwarded json: %s\n",string(csvJSON))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Could not forward request: %v", err)))
		return
	}

	var csvReq *http.Request
	csvReq, err = http.NewRequest("POST", csvURL, bytes.NewReader(csvJSON))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Could not perform HTTP request to database: %v", err)))
		return
	}

	csvReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(csvReq)

	if err != nil {
		goto printerror
	} else {
		cw.Flush()
		if err = cw.Error(); err != nil {
			goto printerror
		}
	}

	var buffer []byte = make([]byte, FORWARD_CHUNKSIZE) // forward the response in 4 KiB chunks

	var bytesRead int
	var readErr error = nil
	for readErr == nil {
		bytesRead, readErr = resp.Body.Read(buffer)
		w.Write(buffer[:bytesRead])
	}

	resp.Body.Close()
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("To log in, make a POST request with JSON containing a username and password."))
		return
	}

	var err error
	var jsonLogin map[string]interface{}
	var usernameint interface{}
	var username string
	var passwordint interface{}
	var password string
	var ok bool

	r.Body = http.MaxBytesReader(w, r.Body, MAX_REQSIZE)
	var loginDecoder *json.Decoder = json.NewDecoder(r.Body)

	err = loginDecoder.Decode(&jsonLogin)
	if err != nil {
		w.Write([]byte(fmt.Sprintf("Error: received invalid JSON: %v", err)))
		return
	}

	usernameint, ok = jsonLogin["username"]
	if !ok {
		w.Write([]byte(fmt.Sprintf("Error: JSON must contain field 'username'")))
		return
	}

	passwordint, ok = jsonLogin["password"]
	if !ok {
		w.Write([]byte(fmt.Sprintf("Error: JSON must contain field 'password'")))
		return
	}

	username, ok = usernameint.(string)
	if !ok {
		w.Write([]byte(fmt.Sprintf("Error: field 'username' must be a string")))
		return
	}

	password, ok = passwordint.(string)
	if !ok {
		w.Write([]byte(fmt.Sprintf("Error: field 'password' must be a string")))
		return
	}

	tokenarr := userlogin(accountConn, username, []byte(password))
	if tokenarr != nil {
		token64buf := make([]byte, token64len)
		base64.StdEncoding.Encode(token64buf, tokenarr)
		w.Write(token64buf)
	}
	// else: invalid credentials, so respond with nothing
}

func parseToken(reader io.Reader) []byte {
	tokenencoded := make([]byte, token64len, token64len)
	tokenslice := make([]byte, token64dlen, token64dlen)

	n, err := io.ReadFull(reader, tokenencoded)
	if err == nil && n == token64len {
		n, err = base64.StdEncoding.Decode(tokenslice, tokenencoded)
		if n == TOKEN_BYTE_LEN && err == nil {
			return tokenslice
		}
	}

	return nil
}

func logoffHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("To log off, make a POST request with the session token."))
		return
	}

	// parseToken doesn't buffer the whole request in memory, so we don't need to use http.MaxBytesReader
	tokenslice := parseToken(r.Body)

	if tokenslice != nil && userlogoff(tokenslice) {
		w.Write([]byte("Logoff successful."))
	} else {
		w.Write([]byte("Invalid session token."))
	}
}

func changepwHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("To change password, make a POST request with the appropriate JSON document."))
		return
	}

	var err error
	var jsonChangePassword map[string]interface{}
	var tokenint interface{}
	var token string
	var oldpasswordint interface{}
	var oldpassword string
	var newpasswordint interface{}
	var newpassword string
	var ok bool
	var tokenslice []byte

	r.Body = http.MaxBytesReader(w, r.Body, MAX_REQSIZE)
	var pwDecoder *json.Decoder = json.NewDecoder(r.Body)

	err = pwDecoder.Decode(&jsonChangePassword)
	if err != nil {
		w.Write([]byte(fmt.Sprintf("Error: received invalid JSON: %v", err)))
		return
	}

	tokenint, ok = jsonChangePassword["token"]
	if !ok {
		w.Write([]byte("Error: JSON must contain field 'token'"))
		return
	}

	oldpasswordint, ok = jsonChangePassword["oldpassword"]
	if !ok {
		w.Write([]byte("Error: JSON must contain field 'oldpassword'"))
		return
	}

	newpasswordint, ok = jsonChangePassword["newpassword"]
	if !ok {
		w.Write([]byte("Error: JSON must contain field 'newpassword'"))
		return
	}

	token, ok = tokenint.(string)
	if !ok {
		w.Write([]byte("Error: field 'token' must be a string"))
		return
	}

	oldpassword, ok = oldpasswordint.(string)
	if !ok {
		w.Write([]byte("Error: field 'oldpassword' must be a string"))
		return
	}

	newpassword, ok = newpasswordint.(string)
	if !ok {
		w.Write([]byte("Error: field 'newpassword' must be a string"))
		return
	}

	if len(token) != token64len {
		w.Write([]byte(ERROR_INVALID_TOKEN))
		return
	}

	tokenslice, err = base64.StdEncoding.DecodeString(token)
	if err != nil {
		w.Write([]byte(ERROR_INVALID_TOKEN))
		return
	}

	success := userchangepassword(accountConn, tokenslice, []byte(oldpassword), []byte(newpassword))
	w.Write([]byte(success))
}

func checktokenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("To check a token, make a POST request with the token in the request body."))
		return
	}

	tokenslice := parseToken(r.Body)

	if tokenslice != nil && getloginsession(tokenslice) != nil {
		w.Write([]byte("ok"))
	} else {
		w.Write([]byte(ERROR_INVALID_TOKEN))
	}
}
