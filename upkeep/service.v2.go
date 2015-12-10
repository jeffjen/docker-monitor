package upkeep

import (
	pxy "github.com/jeffjen/ambd/ambctl/arg"

	log "github.com/Sirupsen/logrus"
	docker "github.com/fsouza/go-dockerclient"
	dri "github.com/jeffjen/docker-monitor/driver"
	disc "github.com/jeffjen/go-discovery"
	node "github.com/jeffjen/go-discovery/info"

	etcd "github.com/coreos/etcd/client"
	ctx "golang.org/x/net/context"

	"fmt"
	"os"
	"path"
	"time"
)

func init() {
	var level = os.Getenv("LOG_LEVEL")
	switch level {
	case "DEBUG":
		log.SetLevel(log.DebugLevel)
		break
	case "INFO":
		log.SetLevel(log.InfoLevel)
		break
	case "WARNING":
		log.SetLevel(log.WarnLevel)
		break
	case "ERROR":
		log.SetLevel(log.ErrorLevel)
		break
	case "FATAL":
		log.SetLevel(log.FatalLevel)
		break
	case "PANIC":
		log.SetLevel(log.PanicLevel)
		break
	default:
		log.SetLevel(log.InfoLevel)
		break
	}
}

const (
	UpkeepTimeout = 250 * time.Millisecond

	ProbeTimeout = 100 * time.Millisecond

	MaxFailAttemps = 3
)

type Service struct {
	Hb  time.Duration `json: "Heartbeat"`
	TTL time.Duration `json: "TTL"`

	PHb           time.Duration `json: "ProbeHeartbeat"`
	ProbeType     string        `json: "ProbeType"`
	ProbeEndpoint string        `json: "ProbeEndpoint"`

	Id       string           `json: "ContainerID"`
	Srv      string           `json: "Service"`
	Port     string           `json: "Port"`
	Net      []docker.APIPort `json: "Net"`
	Proxy    []pxy.Info       `json: "Proxy"`
	ProxyCfg string           `json: "ProxyCfg"`
}

type srv struct {
	*Service // embedded type

	key []string

	kAPI etcd.KeysAPI
	opts *etcd.SetOptions

	driver dri.Driver
}

func (serv *srv) keep(c ctx.Context) (resp <-chan error) {
	output := make(chan error, 1)
	go func() {
		var err error
		for _, k := range serv.key {
			_, err = serv.kAPI.Set(c, k, node.MetaData, serv.opts)
			if err == nil {
				serv.opts.PrevExist = etcd.PrevExist
			} else {
				serv.opts.PrevExist = etcd.PrevIgnore
				break
			}
		}
		log.WithFields(log.Fields{"err": err, "key": serv.key}).Debug("keep")
		output <- err
	}()
	return output
}

func (serv *srv) probe(c ctx.Context) (resp <-chan error) {
	output := make(chan error, 1)
	go func() {
		var err error
		if err = serv.driver.Probe(c); err != nil {
			output <- err
		} else {
			output <- nil
		}
		log.WithFields(log.Fields{"err": err, "srv": serv.Srv}).Debug("probe")
	}()
	return output
}

func Get(iden string) (s *Service) {
	s, _ = rec.Get(iden).(*Service)
	return
}

func Place(service *Service) {
	if r, ok := Record[service.Id]; ok {
		r.Abort()
	}
	rec.Set(service.Id, service)
	Register(service)
}

func Register(service *Service) {
	alloc := AllocHelper(service.ProbeType)

	serv := &srv{
		service,
		make([]string, 0),
		etcd.NewKeysAPI(disc.NewDiscovery()),
		&etcd.SetOptions{TTL: service.TTL},
		nil,
	}

	logger := log.WithFields(log.Fields{
		"ID": serv.Id[:12], "srv": serv.Srv, "heartbeat": serv.Hb, "ttl": serv.TTL,
	})

	// Advertise key on the discovery service
	if serv.Port != "" {
		serv.key = append(serv.key, fmt.Sprintf("%s/%s:%s", serv.Srv, Advertise, serv.Port))
	} else if len(serv.Net) > 0 {
		serv.key = make([]string, 0)
		for _, p := range serv.Net {
			if p.PublicPort != 0 && p.IP == "0.0.0.0" {
				serv.key = append(serv.key, fmt.Sprintf("%s/%s:%d", serv.Srv, Advertise, p.PublicPort))
			}
		}
	}

	var endpoint string
	if serv.ProbeEndpoint == "" {
		endpoint = path.Base(serv.key[0])
	} else {
		endpoint = fmt.Sprintf("%s/%s", path.Base(serv.key[0]), serv.ProbeEndpoint)
	}

	// TODO:  setup driver for probing
	driver, drr := alloc(endpoint)
	if drr != nil {
		logger.WithFields(log.Fields{"err": drr}).Error("-register")
		return
	}
	serv.driver = driver
	logger.Debug("+register")

	c, abort := ctx.WithCancel(RootContext)
	go func() {
		// Request to establish proxy port to ambassador
		openProxyConfig(serv.ProxyCfg, serv.Proxy)

		// setup work cycle
		heartbeat, probe := time.NewTicker(serv.Hb), time.NewTicker(serv.PHb)
		defer func() {
			heartbeat.Stop()
			probe.Stop()
		}()

		logger.Info("start")
		func() {
			d, abort := ctx.WithTimeout(c, UpkeepTimeout)
			select {
			case <-d.Done():
				logger.WithFields(log.Fields{"err": d.Err()}).Error("*up")
			case e := <-serv.keep(d):
				if e != nil {
					logger.WithFields(log.Fields{"err": e}).Error("-up")
				} else {
					logger.Info("+up")
				}
			}
			abort() // release
		}()

		var chk = NewFail(MaxFailAttemps)
		for yay := true; yay; {
			select {
			case <-heartbeat.C:
				if !chk.Pass() {
					serv.opts.PrevExist = etcd.PrevIgnore
					logger.Error("!up")
				} else {
					d, abort := ctx.WithTimeout(c, UpkeepTimeout)
					select {
					case <-d.Done():
						logger.WithFields(log.Fields{"err": d.Err()}).Error("*up")
					case e := <-serv.keep(d):
						if e != nil {
							logger.WithFields(log.Fields{"err": e}).Error("-up")
						} else {
							logger.Info("+up")
						}
					}
					abort() // release
				}

			case <-probe.C:
				d, abort := ctx.WithTimeout(c, ProbeTimeout)
				select {
				case <-d.Done():
					count := chk.Bad()
					logger.WithFields(log.Fields{"err": d.Err(), "fail": count}).Warning("*probe")
				case e := <-serv.probe(d):
					if e != nil {
						count := chk.Bad()
						logger.WithFields(log.Fields{"err": e, "fail": count}).Warning("-probe")
					} else {
						chk.Good()
						logger.Info("+probe")
					}
				}
				abort() // release

			case <-c.Done():
				logger.Warning("down")
				yay = false
			}
		}
	}()

	Record[serv.Id] = &RunningRecord{serv.Srv, abort} // register abort function for this service
}

func Suspend(iden string) {
	if r, ok := Record[iden]; ok {
		r.Abort()
	}
}

func Unregister(iden string) {
	if r, ok := Record[iden]; ok {
		delete(Record, iden)
		r.Abort()
		rec.Del(iden)
		log.WithFields(log.Fields{"ID": iden[:12], "srv": r.Srv}).Warning("die")
	}
}