//go:build !linux || !katl_vmtest

package main

import "context"

type managementExpiryHooks struct{}

type managementExpiryTrial struct{}

func (managementExpiryHooks) start(ctx context.Context, cancel context.CancelFunc) (context.Context, context.CancelFunc, managementExpiryTrial) {
	return ctx, cancel, managementExpiryTrial{}
}

func (managementExpiryTrial) observeOffline(string) {}

func (managementExpiryTrial) report(error) {}
