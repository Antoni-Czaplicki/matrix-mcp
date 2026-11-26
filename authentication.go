package main

import (
	"context"
	"errors"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func newMatrixClient(ctx context.Context, cfg Config) (*mautrix.Client, error) {
	client, err := mautrix.NewClient(cfg.Homeserver, "", cfg.AccessToken)
	if err != nil || cfg.AccessToken == "" {
		return client, err
	}

	session, err := client.Whoami(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify Matrix session: %w", err)
	}
	if session == nil || session.UserID != id.UserID(cfg.UserID) {
		return nil, errors.New("Matrix session does not belong to MATRIX_USER_ID")
	}
	if session.DeviceID == "" {
		return nil, errors.New("Matrix session must be associated with a device for encryption")
	}
	client.UserID = session.UserID
	client.DeviceID = session.DeviceID
	return client, nil
}
