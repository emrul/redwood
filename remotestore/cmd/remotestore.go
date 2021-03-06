package main

import (
	rw "github.com/brynbellomy/redwood"
	"github.com/brynbellomy/redwood/ctx"
	"github.com/brynbellomy/redwood/remotestore"
)

type app struct {
	ctx.Context
}

func main() {
	addr1, err := rw.AddressFromHex("96216849c49358b10257cb55b28ea603c874b05e")
	if err != nil {
		panic(err)
	}
	addr2, err := rw.AddressFromHex("bd2eeb9c7dbe50264d528541c9d52142b616f55a")
	if err != nil {
		panic(err)
	}
	server := remotestore.NewServer("tcp", ":4567", "/tmp/badger-remote", []rw.Address{addr1, addr2})

	app := app{}
	app.CtxAddChild(server.Ctx(), nil)
	err = app.CtxStart(
		func() error { return server.Start() },
		nil,
		nil,
		nil,
	)

	if err != nil {
		panic(err)
	}

	app.AttachInterruptHandler()
	app.CtxWait()
}
