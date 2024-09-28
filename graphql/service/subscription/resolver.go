/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2023, daeuniverse Organization <team@v2raya.org>
 */

package subscription

import (
	"github.com/daeuniverse/dae-wing/common"
	"github.com/daeuniverse/dae-wing/db"
	"github.com/daeuniverse/dae-wing/graphql/service/node"
	"github.com/graph-gophers/graphql-go"
)

type Resolver struct {
	*db.Subscription
}

func (r *Resolver) ID() graphql.ID {
	return common.EncodeCursor(r.Subscription.ID)
}
func (r *Resolver) UpdatedAt() graphql.Time {
	return graphql.Time{
		Time: r.Subscription.UpdatedAt,
	}
}
func (r *Resolver) Tag() *string {
	return r.Subscription.Tag
}
func (r *Resolver) Link() string {
	return r.Subscription.Link
}
func (r *Resolver) CronExp() string {
	return r.Subscription.CronExp
}
func (r *Resolver) CronEnable() bool {
	return r.Subscription.CronEnable
}
func (r *Resolver) Status() string {
	return r.Subscription.Status
}
func (r *Resolver) Info() string {
	return r.Subscription.Info
}
func (r *Resolver) Nodes(args *struct {
	First *int32
	After *graphql.ID
}) (*node.ConnectionResolver, error) {
	id := common.EncodeCursor(r.Subscription.ID)
	return node.NewConnectionResolver(nil, &id, args.First, args.After)
}
