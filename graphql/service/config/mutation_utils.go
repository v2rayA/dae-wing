/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2023, daeuniverse Organization <team@v2raya.org>
 */

package config

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/daeuniverse/dae-wing/common"
	"github.com/daeuniverse/dae-wing/dae"
	"github.com/daeuniverse/dae-wing/db"
	"github.com/daeuniverse/dae-wing/graphql/service/config/global"
	daeConfig "github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/graph-gophers/graphql-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func Create(ctx context.Context, name string, glob *global.Input) (*Resolver, error) {
	if glob == nil {
		glob = &global.Input{}
	}
	strGlobal, err := glob.Marshal()
	if err != nil {
		return nil, err
	}
	m := db.Config{
		ID:       0,
		Name:     name,
		Global:   strGlobal,
		Selected: false,
	}
	// Check grammar and to dae config.
	c, err := dae.ParseConfig(&m.Global, nil, nil)
	if err != nil {
		return nil, err
	}
	if err = db.DB(ctx).Create(&m).Error; err != nil {
		return nil, err
	}
	return &Resolver{
		DaeGlobal: &c.Global,
		Model:     &m,
	}, nil
}

func Update(ctx context.Context, _id graphql.ID, inputGlobal global.Input) (*Resolver, error) {
	id, err := common.DecodeCursor(_id)
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
	var m db.Config
	if err = tx.Model(&db.Config{}).Where("id = ?", id).First(&m).Error; err != nil {
		return nil, err
	}
	// Prepare to partially update.
	// Convert global string in database to daeConfig.Global.
	c, err := dae.ParseConfig(&m.Global, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("bad current config: %w", err)
	}
	// Assign input items to daeConfig.Global.
	inputGlobal.Assign(&c.Global)
	// Marshal back to string.
	marshaller := daeConfig.Marshaller{IndentSpace: 2}
	if err = marshaller.MarshalSection("global", reflect.ValueOf(c.Global), 0); err != nil {
		return nil, err
	}
	// Update.
	if err = tx.Model(&db.Config{ID: id}).Updates(map[string]interface{}{
		"global":  string(marshaller.Bytes()),
		"version": gorm.Expr("version + 1"),
	}).Error; err != nil {
		return nil, err
	}
	return &Resolver{
		DaeGlobal: &c.Global,
		Model:     &m,
	}, nil
}

func Remove(ctx context.Context, _id graphql.ID) (n int32, err error) {
	id, err := common.DecodeCursor(_id)
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
	m := db.Config{ID: id}
	q := tx.Clauses(clause.Returning{Columns: []clause.Column{{Name: "selected"}}}).
		Select(clause.Associations).
		Delete(&m)
	if q.Error != nil {
		return 0, q.Error
	}
	// Check if the config to delete is selected.
	if q.RowsAffected > 0 && m.Selected {
		// Check if dae is running.
		var sys db.System
		if err = tx.Model(&db.System{}).FirstOrCreate(&sys).Error; err != nil {
			return 0, err
		}
		if sys.Running {
			// Stop running with dry-run.
			if _, err = Run(tx, true); err != nil {
				return 0, err
			}
		}
	}
	return int32(q.RowsAffected), nil
}

type node struct {
	dbNode     *db.Node
	groups     []*db.Group
	uniqueName string
}

func deduplicateNodes(nodes []*node) []*node {
	set := make(map[string]*node)
	for _, node := range nodes {
		if oldNode, ok := set[node.dbNode.Link]; ok {
			oldNode.groups = append(oldNode.groups, node.groups...)
		} else {
			set[node.dbNode.Link] = node
		}
	}
	ret := make([]*node, 0, len(set))
	for _, node := range set {
		ret = append(ret, node)
	}
	return ret
}

func uniquefyNodesName(nodes []*node) {
	// Uniquefy names of nodes.
	// Sort nodes by "has node.Tag" because node.Tag is unique but names of others may be the same with them.
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodes[i].dbNode.Tag != nil && nodes[j].dbNode.Tag == nil
	})
	nameToNodes := make(map[string]*node)
	for i := range nodes {
		node := nodes[i]
		if node.dbNode.Tag != nil {
			nameToNodes[*node.dbNode.Tag] = node
		} else {
			baseName := node.dbNode.Name
			if node.dbNode.SubscriptionID != nil {
				baseName = fmt.Sprintf("%v.%v", *node.dbNode.SubscriptionID, baseName)
			}
			// SubID.Name
			wantedName := baseName
			for j := 0; ; j++ {
				_, exist := nameToNodes[wantedName]
				if !exist {
					nameToNodes[wantedName] = node
					break
				}
				// SubID.Name.1
				wantedName = fmt.Sprintf("%v.%v", baseName, j)
			}
		}
	}
	for name, node := range nameToNodes {
		node.uniqueName = name
	}
}

func Select(ctx context.Context, _id graphql.ID) (n int32, err error) {
	id, err := common.DecodeCursor(_id)
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
	// Unset all selected.
	q := tx.Model(&db.Config{}).Where("selected = ?", true).Update("selected", false)
	if err = q.Error; err != nil {
		return 0, err
	}
	isReplace := q.RowsAffected > 0
	// Set selected.
	q = tx.Model(&db.Config{ID: id}).Update("selected", true)
	if err = q.Error; err != nil {
		return 0, err
	}
	if q.RowsAffected == 0 {
		return 0, fmt.Errorf("no such config")
	}
	if isReplace {
		// Check if dae is running.
		var sys db.System
		if err = tx.Model(&db.System{}).FirstOrCreate(&sys).Error; err != nil {
			return 0, err
		}
		if sys.Running {
			// Run with new config.
			if _, err = Run(tx, false); err != nil {
				return 0, err
			}
		}
	}
	return 1, nil
}

func Rename(ctx context.Context, _id graphql.ID, name string) (n int32, err error) {
	id, err := common.DecodeCursor(_id)
	if err != nil {
		return 0, err
	}
	q := db.DB(ctx).Model(&db.Config{ID: id}).
		Update("name", name)
	if q.Error != nil {
		return 0, q.Error
	}
	return int32(q.RowsAffected), nil
}

func Run(d *gorm.DB, noLoad bool) (n int32, err error) {
	//// Dry run.
	if noLoad {
		ch := make(chan error)
		dae.ChReloadConfigs <- &dae.ReloadMessage{
			Config:   dae.EmptyConfig,
			Callback: ch,
		}
		err = <-ch
		if err != nil {
			return 0, fmt.Errorf("failed to dryrun: %w; see more in log and report bugs", err)
		}

		// Running -> false
		var sys db.System
		if err = d.Model(&db.System{}).FirstOrCreate(&sys).Error; err != nil {
			return 0, err
		}
		if err = d.Model(&sys).Updates(map[string]interface{}{
			"running": false,
		}).Error; err != nil {
			return 0, err
		}
		return 1, nil
	}

	//// Run selected global+dns+routing.
	/// Get them from database and parse them to daeConfig.
	var mConfig db.Config
	var mDns db.Dns
	var mRouting db.Routing
	q := d.Model(&db.Config{}).Where("selected = ?", true).First(&mConfig)
	if (q.Error == nil && q.RowsAffected == 0) || errors.Is(q.Error, gorm.ErrRecordNotFound) {
		return 0, fmt.Errorf("please select a config")
	}
	if q.Error != nil {
		return 0, q.Error
	}
	q = d.Model(&db.Dns{}).Where("selected = ?", true).First(&mDns)
	if (q.Error == nil && q.RowsAffected == 0) || errors.Is(q.Error, gorm.ErrRecordNotFound) {
		return 0, fmt.Errorf("please select a dns")
	}
	if q.Error != nil {
		return 0, q.Error
	}
	q = d.Model(&db.Routing{}).Where("selected = ?", true).First(&mRouting)
	if (q.Error == nil && q.RowsAffected == 0) || errors.Is(q.Error, gorm.ErrRecordNotFound) {
		return 0, fmt.Errorf("please select a routing")
	}
	if q.Error != nil {
		return 0, q.Error
	}
	c, err := dae.ParseConfig(&mConfig.Global, &mDns.Dns, &mRouting.Routing)
	if err != nil {
		return 0, err
	}
	/// Fill in necessary groups and nodes.
	// Find groups needed by routing.
	outbounds := dae.NecessaryOutbounds(&c.Routing)
	var groups []db.Group
	q = d.Model(&db.Group{}).
		Where("name in ?", outbounds).
		Preload("PolicyParams").
		Preload("Subscription").
		Preload("Subscription.Node").
		Find(&groups)
	if q.Error != nil {
		return 0, q.Error
	}
	if q.RowsAffected != int64(len(outbounds)) {
		// Find not found.
		nameSet := map[string]struct{}{}
		for _, name := range outbounds {
			nameSet[name] = struct{}{}
		}
		for _, g := range groups {
			delete(nameSet, g.Name)
		}
		var notFound []string
		for name := range nameSet {
			switch name {
			case "direct", "block", "must_direct":
				// Preset groups.
			default:
				notFound = append(notFound, name)
			}
		}
		if len(notFound) > 0 {
			return 0, fmt.Errorf("groups not defined but referenced by routing: %v", strings.Join(notFound, ", "))
		}
	}
	// Find nodes in groups.
	var nodes []*node
	for _, g := range groups {
		g := g
		for _, gsub := range g.Subscription {
			for _, n := range gsub.Node {
				n := n
				nodes = append(nodes, &node{
					dbNode: &n,
					groups: []*db.Group{&g},
				})
			}
		}
		var solitaryNodes []db.Node
		if err = d.Model(g).
			Association("Node").
			Find(&solitaryNodes, "subscription_id is null"); err != nil {
			return 0, err
		}
		for _, n := range solitaryNodes {
			n := n
			nodes = append(nodes, &node{
				dbNode: &n,
				groups: []*db.Group{&g},
			})
		}
	}
	nodes = deduplicateNodes(nodes)
	uniquefyNodesName(nodes)
	// Group -> nodes
	mGroupNode := make(map[*db.Group][]*node)
	for _, node := range nodes {
		for _, group := range node.groups {
			mGroupNode[group] = append(mGroupNode[group], node)
		}
	}
	// Fill in group section.
	for g, nodes := range mGroupNode {
		// Parse policy.
		var policy daeConfig.FunctionListOrString
		if len(g.PolicyParams) == 0 {
			policy = g.Policy
		} else {
			// Parse policy params.
			var params []*config_parser.Param
			for _, param := range g.PolicyParams {
				params = append(params, param.Marshal())
			}
			policy = &config_parser.Function{
				Name:   g.Policy,
				Not:    false,
				Params: params,
			}
		}
		// Node names to filter.
		var names []*config_parser.Param
		for _, node := range nodes {
			names = append(names, &config_parser.Param{
				Val: node.uniqueName,
			})
		}
		c.Group = append(c.Group, daeConfig.Group{
			Name: g.Name,
			Filter: []*config_parser.Function{{
				Name:   "name",
				Not:    false,
				Params: names,
			}},
			Policy: policy,
		})
	}
	// Fill in node section.
	for _, node := range nodes {
		c.Node = append(c.Node, daeConfig.KeyableString(fmt.Sprintf("%v:%v", node.uniqueName, node.dbNode.Link)))
	}

	/// Reload with current config.
	chReloadCallback := make(chan error)
	dae.ChReloadConfigs <- &dae.ReloadMessage{
		Config:   c,
		Callback: chReloadCallback,
	}
	errReload := <-chReloadCallback
	if errReload != nil {
		return 0, fmt.Errorf("failed to load new config: %w; see more in log", errReload)
	}

	// Save running status
	var sys db.System
	if err = d.Model(&db.System{}).FirstOrCreate(&sys).Error; err != nil {
		return 0, err
	}
	var groupVersions []string
	for _, g := range groups {
		groupVersions = append(groupVersions, strconv.FormatUint(uint64(g.Version), 10))
	}
	if err = d.Model(&sys).Updates(map[string]interface{}{
		"running":                 true,
		"running_config_id":       mConfig.ID,
		"running_config_version":  mConfig.Version,
		"running_dns_id":          mDns.ID,
		"running_dns_version":     mDns.Version,
		"running_routing_id":      mRouting.ID,
		"running_routing_version": mRouting.Version,
		"running_group_versions":  strings.Join(groupVersions, ","),
	}).Error; err != nil {
		return 0, err
	}
	if err = d.Model(&sys).Association("RunningGroups").Replace(groups); err != nil {
		return 0, err
	}

	return 1, nil
}
