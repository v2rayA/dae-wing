/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2023, daeuniverse Organization <team@v2raya.org>
 */

package subscription

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/daeuniverse/dae-wing/common"
	"github.com/daeuniverse/dae-wing/dae"
	"github.com/daeuniverse/dae-wing/db"
	"github.com/daeuniverse/dae-wing/graphql/internal"
	"github.com/daeuniverse/dae-wing/graphql/service/node"
	"github.com/daeuniverse/dae/common/subscription"
	"github.com/go-co-op/gocron"
	"github.com/graph-gophers/graphql-go"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ImportResult struct {
	Link             string
	NodeImportResult []*node.ImportResult
	Sub              *Resolver
}

func fetchLinks(subscriptionLink string) (links []string, err error) {
	timeout := 10 * time.Second
	// Try with direct by default.
	links, err = _fetchLinks(subscriptionLink, http.DefaultTransport, timeout/2)
	if err != nil {
		// Retry with dae routing.
		links, err2 := _fetchLinks(subscriptionLink, dae.HttpTransport, timeout/2)
		if err2 != nil {
			if errors.Is(err2, dae.ErrControlPlaneNotInit) {
				return nil, err
			} else {
				return nil, fmt.Errorf("%v (direct); %w (route)", err, err2)
			}
		}
		return links, nil
	}
	return links, nil
}

func _fetchLinks(subscriptionLink string, transport http.RoundTripper, timeout time.Duration) (links []string, err error) {
	/// Resolve subscription to node links.
	// Fetch subscription link.
	var (
		b    []byte
		resp *http.Response
	)
	c := http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	req, err := http.NewRequest("GET", subscriptionLink, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("%v/%v (like v2rayA/1.0 WebRequestHelper) (like v2rayN/1.0 WebRequestHelper)", db.AppName, db.AppVersion))
	resp, err = c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to fetch link: %v", resp.Status)
	}
	defer resp.Body.Close()
	b, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Resolve node links.
	noLogger := logrus.New()
	noLogger.SetOutput(io.Discard)
	links, err = subscription.ResolveSubscriptionAsSIP008(noLogger, b)
	if err != nil {
		links = subscription.ResolveSubscriptionAsBase64(noLogger, b)
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("fetched but no any node was found")
	}
	return links, nil
}

func Import(c *gorm.DB, rollbackError bool, argument *internal.ImportArgument) (r *ImportResult, err error) {
	if err = argument.ValidateTag(); err != nil {
		return nil, err
	}
	links, err := fetchLinks(argument.Link)
	if err != nil {
		return nil, err
	}
	/// Create a subscription model.
	m := db.Subscription{
		ID:        0,
		UpdatedAt: time.Now(),
		Tag:       argument.Tag,
		Link:      argument.Link,
		Status:    "",
		Info:      "", // not supported yet
		Node:      nil,
	}
	if err = c.Create(&m).Error; err != nil {
		return nil, err
	}
	/// Import nodes.
	// Links to import arguments.
	var args []*internal.ImportArgument
	for _, link := range links {
		args = append(args, &internal.ImportArgument{
			Link: link,
			Tag:  nil,
		})
	}
	// Import nodes.
	result, err := node.Import(c, rollbackError, &m.ID, args)
	if err != nil {
		return nil, err
	}
	hasAnyCandidate := false
	for _, r := range result {
		if r.Error == nil {
			hasAnyCandidate = true
			break
		}
	}
	if !hasAnyCandidate {
		return nil, fmt.Errorf("no any valid node can be imported")
	}
	return &ImportResult{
		Link:             argument.Link,
		NodeImportResult: result,
		Sub: &Resolver{
			Subscription: &m,
		},
	}, nil
}

func AutoUpdateVersionByIds(d *gorm.DB, ids []uint) (err error) {
	var sys db.System
	if err = d.Model(&db.System{}).
		FirstOrCreate(&sys).Error; err != nil {
		return err
	}
	if !sys.Running {
		return nil
	}

	if err = d.Exec(`update groups
                set version = groups.version + 1
                from groups g
                    inner join group_subscriptions
                    on g.system_id = ? and g.id = group_subscriptions.group_id and group_subscriptions.subscription_id in ?
				where g.id = groups.id`, sys.ID, ids).Error; err != nil {
		return err
	}

	return nil
}

var schedulerCache = make(map[uint]*gocron.Scheduler)

func UpdateAll(ctx context.Context) {

	var subs []db.Subscription
	if err := db.DB(ctx).Find(&subs).Error; err != nil {
		logrus.Error(err)
		return
	}
	for _, sub := range subs {
		AddUpdateScheduler(ctx, sub.ID)
	}
}

func AddUpdateScheduler(ctc context.Context, id uint) {
	var sub db.Subscription
	if err := db.DB(ctc).Where("id = ?", id).First(&sub).Error; err != nil {
		logrus.Error(err)
		return
	}
	if sub.CronEnable && schedulerCache[sub.ID] == nil {
		s := gocron.NewScheduler(time.Local)
		logrus.Info("Subscription " + *sub.Tag + " update task enabled, with exp " + sub.CronExp)
		s.Cron(sub.CronExp).Do(func() {
			if _, err := UpdateById(ctc, sub.ID); err != nil {
				logrus.Error(err)
			}
		})
		s.StartAsync()
		schedulerCache[sub.ID] = s
	}
}

func RemoveUpdateScheduler(id uint) {
	if schedulerCache[id] != nil {
		logrus.Info("Subscription " + string(id) + " update task disabled")
		schedulerCache[id].Stop()
		delete(schedulerCache, id)
	}
}

func Update(ctx context.Context, _id graphql.ID) (r *Resolver, err error) {
	subId, err := common.DecodeCursor(_id)
	if err != nil {
		return nil, err
	}
	var m *db.Subscription
	m, err = UpdateById(ctx, subId)
	if err != nil {
		return nil, err
	}
	return &Resolver{Subscription: m}, nil
}

func UpdateById(ctx context.Context, subId uint) (sub *db.Subscription, err error) {
	// Fetch node links.
	var m db.Subscription
	if err = db.DB(ctx).Where(&db.Subscription{ID: subId}).First(&m).Error; err != nil {
		return nil, err
	}
	links, err := fetchLinks(m.Link)
	if err != nil {
		return nil, err
	}

	tx := db.BeginTx(ctx)
	defer func() {
		if err == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}()
	// Remove those nodes whose subscription are independent from any groups.
	subQuery := tx.Raw(`select nodes.id as id
                from nodes
                inner join group_nodes on group_nodes.node_id = nodes.id
                where subscription_id = ?`, subId)

	if err = tx.Where("subscription_id = ?", subId).
		Where("id not in (?)", subQuery).
		Select(clause.Associations).
		Delete(&db.Node{}).Error; err != nil {
		return nil, err
	}
	// Import node links.
	var args []*internal.ImportArgument
	for _, link := range links {
		args = append(args, &internal.ImportArgument{Link: link})
	}
	result, err := node.Import(tx, false, &subId, args)
	if err != nil {
		return nil, err
	}
	hasAnyCandidate := false
	for _, r := range result {
		if r.Error == nil {
			hasAnyCandidate = true
			break
		}
	}
	if !hasAnyCandidate {
		return nil, fmt.Errorf("interrupt to update subscription: no any valid node can be imported")
	}
	// Update updated_at and return the latest version.
	if err = tx.Model(&m).
		Clauses(clause.Returning{}).
		Where(&db.Subscription{ID: subId}).
		Update("updated_at", time.Now()).Error; err != nil {
		return nil, err
	}

	// Update modified if subscription is referenced by running config.
	if err = AutoUpdateVersionByIds(tx, []uint{subId}); err != nil {
		return nil, err
	}
	return &m, nil
}

func Remove(ctx context.Context, _ids []graphql.ID) (n int32, err error) {
	ids, err := common.DecodeCursorBatch(_ids)
	if err != nil {
		return 0, err
	}
	tx := db.BeginTx(ctx)
	defer func() {
		if err == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}()
	var nodes []db.Node
	if err = tx.Where("subscription_id in ?", ids).
		Find(&nodes).Error; err != nil {
		return 0, err
	}
	var nodeIds []uint
	for _, n := range nodes {
		nodeIds = append(nodeIds, n.ID)
	}

	// Update modified if any subscriptions are referenced by running config.
	if err = node.AutoUpdateVersionByIds(tx, nodeIds); err != nil {
		return 0, err
	}
	if err = AutoUpdateVersionByIds(tx, ids); err != nil {
		return 0, err
	}

	// Remove.
	if err = tx.Where("subscription_id in ?", ids).
		Delete(&db.Node{}).Error; err != nil {
		return 0, err
	}
	q := tx.Where("id in ?", ids).
		Select(clause.Associations).
		Delete(&db.Subscription{})
	if q.Error != nil {
		return 0, q.Error
	}

	for _, id := range ids {
		RemoveUpdateScheduler(id)
	}

	return int32(q.RowsAffected), nil
}

func Tag(ctx context.Context, _id graphql.ID, tag string) (n int32, err error) {
	if err = common.ValidateTag(tag); err != nil {
		return 0, err
	}
	id, err := common.DecodeCursor(_id)
	if err != nil {
		return 0, err
	}
	q := db.DB(ctx).Model(&db.Subscription{}).
		Where("id = ?", id).
		Update("tag", tag)
	if q.Error != nil {
		return 0, q.Error
	}
	return int32(q.RowsAffected), nil
}
