package cmd

import (
	api "github.com/jeffjen/docker-monitor/api"

	log "github.com/Sirupsen/logrus"
)

func runAPIEndpoint(addr string, stop chan<- struct{}) {
	defer close(stop)

	server := api.GetServer()

	server.Addr = addr
	log.Error(server.ListenAndServe())
}