package proxy

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ApiServer struct {
	Enable     bool
	Apis       map[string]*Api
	manager    *ApiServerManager
	ConfDir    string
	Rw         sync.RWMutex
	routers    *Routers
	web        *WebAdmin
	ServerConf *ServerConfItem
}

func NewApiServer(conf *ServerConfItem, manager *ApiServerManager) *ApiServer {
	apiServer := &ApiServer{ServerConf: conf, manager: manager}
	apiServer.ConfDir = fmt.Sprintf("%s/api_%d", filepath.Dir(manager.ConfPath), conf.Port)
	apiServer.Apis = make(map[string]*Api)
	apiServer.routers = NewRouters()
	apiServer.web = NewWebAdmin(apiServer)
	return apiServer
}

func (apiServer *ApiServer) Start() error {
	addr := fmt.Sprintf(":%d", apiServer.ServerConf.Port)

	apiServer.loadAllApis()
	log.Println("start server:", addr)
	err := http.ListenAndServe(addr, apiServer)
	return err
}

func (apiServer *ApiServer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	router := apiServer.routers.GetRouterByReqPath(req.URL.Path)
	if router != nil {
		router.Hander.ServeHTTP(rw, req)
		return
	}
	apiServer.web.ServeHTTP(rw, req)
}

func (apiServer *ApiServer) loadAllApis() {
	fileNames, _ := filepath.Glob(apiServer.ConfDir + "/*.json")
	for _, fileName := range fileNames {
		apiName := strings.TrimRight(filepath.Base(fileName), ".json")
		apiServer.loadApi(apiName)
	}
}

func (apiServer *ApiServer) deleteApi(apiName string) {
	apiServer.Rw.Lock()
	defer apiServer.Rw.Unlock()
	api, has := apiServer.Apis[apiName]
	if !has {
		return
	}
	api.Delete()
	delete(apiServer.Apis, apiName)
}

func (apiServer *ApiServer) newApi(apiName string) *Api {
	return NewApi(apiServer.ConfDir, apiName)
}

func (apiServer *ApiServer) loadApi(apiName string) error {
	apiServer.Rw.Lock()
	defer apiServer.Rw.Unlock()

	api, err := LoadApiByConf(apiServer.ConfDir, apiName)
	if err != nil {
		log.Println("load api failed,", apiName, err)
		return err
	}

	log.Printf("load api [%s] success", apiName)

	apiServer.Apis[apiName] = api
	if api.Enable {
		router := NewRouterItem(apiName, api.Path, apiServer.newHandler(api))
		apiServer.routers.BindRouter(api.Path, router)
	} else {
		log.Printf("api [%s] is not enable,skip", apiName)
	}

	return nil
}

func (apiServer *ApiServer) newHandler(api *Api) func(http.ResponseWriter, *http.Request) {
	bindPath := api.Path
	log.Println(apiServer.ServerConf.Port, api.Name, "bind path [", bindPath, "]")

	return func(rw http.ResponseWriter, req *http.Request) {
		log.Println(req.URL.String())

		relPath := req.URL.Path[len(bindPath):]
		req.Header.Del("Connection")
		body, _ := ioutil.ReadAll(req.Body)
		logData := make(map[string]interface{})

		cpf := NewCallerPrefConfByHttpRequest(req)

		masterHost := api.GetMasterHostName(cpf)

		defer (func() {
			uri := req.URL.Path
			if req.URL.RawQuery != "" {
				uri += "?" + req.URL.RawQuery
			}
			log.Println(apiServer.ServerConf.Port, req.RemoteAddr, req.Method, uri, "master:", masterHost, logData)
		})()

		var wg sync.WaitGroup

		addrInfo := strings.Split(req.RemoteAddr, ":")
		caller := api.Caller.getCallerItemByIp(cpf.Ip)

		for _, api_host := range api.Hosts {
			wg.Add(1)
			go (func(api_host *Host, rw http.ResponseWriter, req *http.Request) {
				defer wg.Done()

				start := time.Now()
				isMaster := masterHost == api_host.Name
				backLog := make(map[string]interface{})
				logData[fmt.Sprintf("host_%s", api_host.Name)] = backLog

				backLog["isMaster"] = isMaster

				if caller.IsHostIgnore(api_host.Name) {
					backLog["ignore"] = true
					return
				}

				urlNew := api_host.Url
				if strings.HasSuffix(urlNew, "/") {
					urlNew += strings.TrimLeft(relPath, "/")
				} else {
					urlNew += relPath
				}
				if req.URL.RawQuery != "" {
					urlNew += "?" + req.URL.RawQuery
				}
				backLog["url"] = urlNew

				reqNew, _ := http.NewRequest(req.Method, urlNew, ioutil.NopCloser(bytes.NewReader(body)))
				copyHeaders(reqNew.Header, req.Header)
				//				if req.Header.Get("Content-Length") != "" {
				//					reqNew.ContentLength = int64(len(body))
				//				}
				reqNew.Header.Set("HTTP_X_FORWARDED_FOR", addrInfo[0])

				httpClient := &http.Client{}
				httpClient.Timeout = time.Duration(api.TimeoutMs) * time.Millisecond
				resp, err := httpClient.Do(reqNew)

				rw.Header().Set("Server", "api-proxy")

				if err != nil {
					log.Println("fetch "+urlNew, err)
					if isMaster {
						rw.WriteHeader(http.StatusBadGateway)
						rw.Header().Set("api-url", urlNew)
						rw.Write([]byte("apiServer error:" + err.Error()))
					}
					return
				}
				defer resp.Body.Close()
				if isMaster {
					for k, vs := range resp.Header {
						for _, v := range vs {
							rw.Header().Add(k, v)
						}
					}
					rw.Header().Set("api-url", urlNew)
					n, err := io.Copy(rw, resp.Body)
					if err != nil {
						log.Println(urlNew, "io.copy:", n, err)
					}
				}

				used := time.Now().Sub(start)
				backLog["used"] = fmt.Sprintf("%.3f ms", float64(used.Nanoseconds())/1e6)
			})(api_host, rw, req)
		}
		wg.Wait()

	}
}

func (apiServer *ApiServer) getApiByName(name string) *Api {
	if api, has := apiServer.Apis[name]; has {
		return api
	}
	return nil
}