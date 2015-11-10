package cmd

import (
	d "github.com/jeffjen/docker-monitor/driver"
	mgo "github.com/jeffjen/docker-monitor/driver/mongodb"
	up "github.com/jeffjen/docker-monitor/upkeep"

	cli "github.com/codegangsta/cli"

	"errors"
)

var (
	Commands = []cli.Command{
		{
			Name:   "mgo",
			Usage:  "Track mongod/mongos instance",
			Flags:  Flags,
			Before: mongodb,
			Action: Monitor,
		},
		{
			Name:   "tcp",
			Usage:  "Track generic tcp ping endpoint",
			Flags:  Flags,
			Before: tcp,
			Action: Monitor,
		},
	}
)

func mongodb(ctx *cli.Context) error {
	// create closure method for generating new driver for endpoint
	up.AllocDriver = func(endpoint string) (d.Driver, error) {
		return mgo.New(endpoint)
	}

	return nil
}

func tcp(ctx *cli.Context) error {
	// create closure method for generating new driver for endpoint
	up.AllocDriver = func(string) (d.Driver, error) {
		return nil, errors.New("Not yet implemented")
	}

	return nil
}