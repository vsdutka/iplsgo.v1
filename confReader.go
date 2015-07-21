// confReader
package main

import (
	"encoding/json"
	"expvar"
	"github.com/vsdutka/expvarmon"
	"github.com/vsdutka/otasker"
	"gopkg.in/errgo.v1"
	"gopkg.in/goracle.v1/oracle"
	"log"
	"os"
	"sync"
	"time"
)

var (
	configReadDuration = expvar.NewFloat("config_read_duration")
)

func init() {
	expvarmon.RegisterVariableInfo("config_read_duration", "Config read duration", "Seconds", "s")
}

type configReader struct {
	cancelWg   sync.WaitGroup
	cancelChan chan bool
}

func (cr *configReader) shutdown() {
	cr.cancelChan <- true
	cr.cancelWg.Wait()
}
func newConfigReader(
	dsn, configName string,
	timeout time.Duration,

	serverCallback func(
		serviceName, serviceDispName string,
		httpPort, httpDebugPort, httpReadTimeout, httpWriteTimeout int,
		httpSsl bool, httpSslCert, httpSslKey,
		httpLogDir string,
		handlersParams []json.RawMessage) error,
) *configReader {

	r := &configReader{cancelChan: make(chan bool)}
	r.cancelWg.Add(1)

	go func(wg *sync.WaitGroup) {

		var (
			err          error
			conn         *oracle.Connection
			cur          *oracle.Cursor
			confNameVar  *oracle.Variable
			hostNameVar  *oracle.Variable
			confLinesVar *oracle.Variable
			buffer       []byte = make([]byte, 10000*256)

			username string
			password string
			sid      string
			hostName string
		)
		defer func() {
			if cur != nil {
				cur.Close()
			}
			if conn != nil {
				if conn.IsConnected() {
					conn.Close()
				}
			}
			wg.Done()
		}()

		//		//TODO решить проблему с тем, что на момент создания читальщика еще нет параметров сервера и невозможно определить имя файла
		//		fileConfLog, err := os.OpenFile(logFileName, os.O_RDWR|os.O_APPEND, 0666)
		//		if err != nil {
		//			fileConfLog, err = os.OpenFile(logFileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		//			if err != nil {
		//				log.Fatalf("Unable to open log file: %s\n", err)
		//			}
		//		}
		//		defer fileConfLog.Close()
		//		confLogger := log.New(fileConfLog, "", log.Ldate|log.Ltime|log.Lmicroseconds)

		username, password, sid = oracle.SplitDSN(dsn)
		if hostName, err = os.Hostname(); err != nil {
			log.Fatalf("Error getting host name: %s\n", err)

		}
		// Коструируем таймер таким, чтобы первое срабатывание произошло сразу
		timer := time.NewTimer(0)

		for {
			select {
			case <-r.cancelChan:
				{
					return
				}
			case <-timer.C:
				{
					timer.Reset(timeout)

					bg := time.Now()
					err = func() (err error) {
						// Конструируем значения параметров по умолчанию
						type _t struct {
							ServiceName      string            `json:"Service.Name"`
							ServiceDispName  string            `json:"Service.DisplayName"`
							HTTPPort         int               `json:"Http.Port"`
							HTTPDebugPort    int               `json:"Http.DebugPort"`
							HTTPReadTimeout  int               `json:"Http.ReadTimeout"`
							HTTPWriteTimeout int               `json:"Http.WriteTimeout"`
							HTTPSsl          bool              `json:"Http.SSL"`
							HTTPSslCert      string            `json:"Http.SSLCert"`
							HTTPSslKey       string            `json:"Http.SSLKey"`
							HTTPLogDir       string            `json:"Http.LogDir"`
							List             []json.RawMessage `json:"Http.Handlers"`
						}
						var appServerConfig _t = _t{
							ServiceName:      "iPLSGo",
							ServiceDispName:  "iPLSGo Server",
							HTTPPort:         10111,
							HTTPDebugPort:    0,
							HTTPReadTimeout:  15000,
							HTTPWriteTimeout: 15000,
							HTTPSsl:          false,
							HTTPSslCert:      "",
							HTTPSslKey:       "",
							HTTPLogDir:       "${app_dir}\\log\\"}

						defer func(a *_t) {
							// Всегда отправляем результат чтения конфигурации
							// Сюда зайдем в любом случае
							defer func() {
								a.List = a.List[:0]
							}()
							err1 := serverCallback(
								a.ServiceName,
								a.ServiceDispName,
								a.HTTPPort,
								a.HTTPDebugPort,
								a.HTTPReadTimeout,
								a.HTTPWriteTimeout,
								a.HTTPSsl,
								a.HTTPSslCert,
								a.HTTPSslKey,
								a.HTTPLogDir,
								a.List,
							)
							if err1 != nil {
								err = err1
							}
						}(&appServerConfig)

						if conn == nil {
							conn, err = oracle.NewConnection(username, password, sid, false)
							if err != nil {
								// Выходим. Прочитать не получиться
								conn = nil
								return errgo.Newf("Unable to read configuration: %s", otasker.UnMask(err))
							}
						} else {
							err = conn.Ping()
							if err != nil {
								conn.Close()
								if cur != nil {
									cur.Close()
									cur = nil
								}
								conn, err = oracle.NewConnection(username, password, sid, false)
								if err != nil {
									// Выходим. Прочитать не получиться
									conn = nil
									return errgo.Newf("Unable to read configuration: %s", otasker.UnMask(err))
								}
							}
						}
						if cur == nil {
							cur = conn.NewCursor()
							if confNameVar, err = cur.NewVar(configName); err != nil {
								return errgo.Newf("error creating variable for %s(%T): %s", configName, configName, err)
							}

							if hostNameVar, err = cur.NewVar(hostName); err != nil {
								return errgo.Newf("error creating variable for %s(%T): %s", hostName, hostName, err)
							}

						}
						var lines []interface{} = make([]interface{}, 10000)
						defer func() { lines = lines[0:0] }()

						if confLinesVar, err = cur.NewArrayVar(oracle.StringVarType, lines, 256); err != nil {
							return errgo.Newf("error creating variable for %s(%T): %s", confLinesVar, confLinesVar, err)
						}
						defer confLinesVar.Free()

						if err = cur.Execute(stm, nil, map[string]interface{}{"ainstance_name": confNameVar, "ahost_name": hostNameVar, "confLines": confLinesVar}); err != nil {
							return errgo.Newf("error executing `c.config`: %s", otasker.UnMask(err))
						}
						defer func() { buffer = buffer[:0] }()
						var lineBuf []byte //= make([]byte, 256)
						for i := 0; i < int(confLinesVar.ArrayLength()); i++ {
							err = confLinesVar.GetValueInto(&lineBuf, uint(i))
							if err != nil {
								return errgo.Newf("cannot get out value for lines: %s", err)
							}
							if i == 0 {
								buffer = lineBuf[:len(lineBuf)]
							} else {
								buffer = append(buffer, lineBuf[:len(lineBuf)]...)
							}
							lineBuf = lineBuf[:0]
						}

						err = json.Unmarshal(buffer, &appServerConfig)
						if err != nil {
							return errgo.Newf("error parsing configuration: %s", err)
						}

						return
					}()

					if err != nil {
						logInfof("Configuration was read in %6.4f seconds with error. Error: %s\n", time.Since(bg).Seconds(), err)
						//confLogger.Printf("Configuration was read in %6.4f seconds with error. Error: %s\n", time.Since(bg).Seconds(), err)
					} else {
						logInfof("Configuration was read in %6.4f seconds\n", time.Since(bg).Seconds())
						//confLogger.Printf("Configuration was read in %6.4f seconds\n", time.Since(bg).Seconds())
					}
					configReadDuration.Set(time.Since(bg).Seconds())
				}
			}
		}
	}(&r.cancelWg)
	return r
}

const stm = `declare
  param_name    sys.owa.vc_arr;
  param_val     sys.owa.vc_arr;
  thePage       sys.htp.htbuf_arr;
  thePageLinesQ integer := 10000;
  start_line    integer := 0;
  confLines     sys.htp.htbuf_arr;
begin
  sys.OWA.init_cgi_env(0, param_name, param_val);
  sys.htp.init;
  c.config(ainstance_name => :ainstance_name, ahost_name => :ahost_name);
  sys.htp.flush;
  sys.OWA.GET_PAGE(thePage, thePageLinesQ);
  /* Пропускаем HTTP заголовок */
  for i in 1..thePageLinesQ
  loop
    if thePage(i) = sys.owa.NL_CHAR then
      start_line := i + 1;
      exit;
    end if;
  end loop;
  /* Формируем результирующий буфер */
  for i in start_line..thePageLinesQ
  loop
    :confLines(i - start_line + 1) := thePage(i);
  end loop;
  dbms_session.modify_package_state(dbms_session.reinitialize);
end;`