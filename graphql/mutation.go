/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2023, daeuniverse Organization <team@v2raya.org>
 */

package graphql

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/daeuniverse/dae-wing/db"
	"github.com/daeuniverse/dae-wing/graphql/internal"
	"github.com/daeuniverse/dae-wing/graphql/service/config"
	"github.com/daeuniverse/dae-wing/graphql/service/config/global"
	"github.com/daeuniverse/dae-wing/graphql/service/dns"
	"github.com/daeuniverse/dae-wing/graphql/service/group"
	"github.com/daeuniverse/dae-wing/graphql/service/node"
	"github.com/daeuniverse/dae-wing/graphql/service/routing"
	"github.com/daeuniverse/dae-wing/graphql/service/subscription"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/graph-gophers/graphql-go"
	"github.com/tidwall/sjson"
)

type MutationResolver struct{}

func (r *MutationResolver) CreateUser(args *struct {
	Username string
	Password string
}) (token string, err error) {
	if len(args.Password) < 6 || strings.IndexFunc(args.Password, unicode.IsLetter) < 0 || strings.IndexFunc(args.Password, unicode.IsNumber) < 0 {
		return "", fmt.Errorf("too weak password; should contain numbers and letters, and no less than 6 in length")
	}
	tx := db.BeginTx(context.TODO())
	defer func() {
		if err == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}()
	// Check if there is already a user.
	n, err := numberUsers(tx)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", fmt.Errorf("a user already exists")
	}
	// Hash password.
	var sec [32]byte
	if _, err = io.ReadFull(rand.Reader, sec[:]); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(sec[:])
	hashedPassword, err := hashPassword([]byte(secret), args.Password)
	if err != nil {
		return "", err
	}
	// Create user.
	if err = tx.Model(&db.User{}).Create(&db.User{
		Username:     args.Username,
		PasswordHash: hashedPassword,
		JwtSecret:    secret,
	}).Error; err != nil {
		return "", err
	}
	// Return token.
	return getToken(tx, args.Username, args.Password)
}
func (r *MutationResolver) SetJsonStorage(ctx context.Context, args *struct {
	Paths  []string
	Values []string
}) (int32, error) {
	u, err := userFromContext(ctx)
	if err != nil {
		return 0, err
	}
	if len(args.Paths) != len(args.Values) {
		return 0, fmt.Errorf("len(paths) != len(values)")
	}
	for i := range args.Paths {
		u.JsonStorage, err = sjson.Set(u.JsonStorage, args.Paths[i], args.Values[i])
		if err != nil {
			return 0, err
		}
	}
	if err = db.DB(context.TODO()).Model(&u).Update("json_storage", u.JsonStorage).Error; err != nil {
		return 0, err
	}
	return int32(len(args.Paths)), nil
}
func (r *MutationResolver) RemoveJsonStorage(ctx context.Context, args *struct {
	Paths *[]string
}) (n int32, err error) {
	u, err := userFromContext(ctx)
	if err != nil {
		return 0, err
	}
	if args.Paths == nil {
		u.JsonStorage = "{}"
		n = 1
	} else {
		for i := range *args.Paths {
			u.JsonStorage, err = sjson.Delete(u.JsonStorage, (*args.Paths)[i])
			if err != nil {
				return 0, err
			}
		}
		n = int32(len(*args.Paths))
	}
	if err = db.DB(context.TODO()).Model(&u).Update("json_storage", u.JsonStorage).Error; err != nil {
		return 0, err
	}
	return n, nil
}
func (r *MutationResolver) UpdateAvatar(ctx context.Context, args *struct {
	Avatar *string
}) (int32, error) {
	u, err := userFromContext(ctx)
	if err != nil {
		return 0, err
	}
	q := db.DB(context.TODO()).Model(&u).Update("avatar", args.Avatar)
	if err = q.Error; err != nil {
		return 0, err
	}
	return int32(q.RowsAffected), nil
}
func (r *MutationResolver) UpdateName(ctx context.Context, args *struct {
	Name *string
}) (int32, error) {
	u, err := userFromContext(ctx)
	if err != nil {
		return 0, err
	}
	q := db.DB(context.TODO()).Model(&u).Update("name", args.Name)
	if err = q.Error; err != nil {
		return 0, err
	}
	return int32(q.RowsAffected), nil
}
func (r *MutationResolver) UpdateUsername(ctx context.Context, args *struct {
	Username string
}) (int32, error) {
	u, err := userFromContext(ctx)
	if err != nil {
		return 0, err
	}
	q := db.DB(context.TODO()).Model(&u).Update("username", args.Username)
	if err = q.Error; err != nil {
		return 0, err
	}
	return int32(q.RowsAffected), nil
}

func UpdatePassword(ctx context.Context, args *struct {
	CurrentPassword string
	NewPassword     string
}, u *db.User, skipVerify bool) (token string, err error) {
	// Check password.
	if !skipVerify {
		hashedPassword, err := hashPassword([]byte(u.JwtSecret), args.CurrentPassword)
		if err != nil {
			return "", err
		}
		if hashedPassword != u.PasswordHash {
			return "", fmt.Errorf("incorrect password")
		}
	}

	// Generate new jwt secret (to log out others) and password hash.
	var sec [32]byte
	if _, err = io.ReadFull(rand.Reader, sec[:]); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(sec[:])
	hashedPassword, err := hashPassword([]byte(secret), args.NewPassword)
	if err != nil {
		return "", err
	}
	tx := db.BeginTx(ctx)
	defer func() {
		if err == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}()
	q := tx.Model(u).Updates(db.User{
		PasswordHash: hashedPassword,
		JwtSecret:    secret,
	})
	if q.Error != nil {
		return "", q.Error
	}

	// Return token.
	return getToken(tx, u.Username, args.NewPassword)
}

func (r *MutationResolver) UpdatePassword(ctx context.Context, args *struct {
	CurrentPassword string
	NewPassword     string
}) (string, error) {
	u, err := userFromContext(ctx)
	if err != nil {
		return "", err
	}
	return UpdatePassword(ctx, args, u, false)
}
func (r *MutationResolver) CreateConfig(args *struct {
	Name   *string
	Global *global.Input
}) (c *config.Resolver, err error) {
	var strName string
	if args.Name != nil {
		strName = *args.Name
	}
	return config.Create(context.TODO(), strName, args.Global)
}

func (r *MutationResolver) UpdateConfig(args *struct {
	ID     graphql.ID
	Global global.Input
}) (*config.Resolver, error) {
	return config.Update(context.TODO(), args.ID, args.Global)
}

func (r *MutationResolver) RenameConfig(args *struct {
	ID   graphql.ID
	Name string
}) (int32, error) {
	return config.Rename(context.TODO(), args.ID, args.Name)
}

func (r *MutationResolver) RemoveConfig(args *struct {
	ID graphql.ID
}) (int32, error) {
	return config.Remove(context.TODO(), args.ID)
}

func (r *MutationResolver) SelectConfig(args *struct {
	ID graphql.ID
}) (int32, error) {
	return config.Select(context.TODO(), args.ID)
}

func (r *MutationResolver) Run(args *struct {
	Dry bool
}) (int32, error) {
	tx := db.BeginTx(context.TODO())
	ret, err := config.Run(tx, args.Dry)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	tx.Commit()
	return ret, nil
}

func (r *MutationResolver) CreateDns(args *struct {
	Name *string
	Dns  *string
}) (c *dns.Resolver, err error) {
	var strDns, strName string
	if args.Dns != nil {
		strDns = *args.Dns
	}
	if args.Name != nil {
		strName = *args.Name
	}
	return dns.Create(context.TODO(), strName, strDns)
}

func (r *MutationResolver) UpdateDns(args *struct {
	ID  graphql.ID
	Dns string
}) (*dns.Resolver, error) {
	return dns.Update(context.TODO(), args.ID, args.Dns)
}

func (r *MutationResolver) RenameDns(args *struct {
	ID   graphql.ID
	Name string
}) (int32, error) {
	return dns.Rename(context.TODO(), args.ID, args.Name)
}

func (r *MutationResolver) RemoveDns(args *struct {
	ID graphql.ID
}) (int32, error) {
	return dns.Remove(context.TODO(), args.ID)
}

func (r *MutationResolver) SelectDns(args *struct {
	ID graphql.ID
}) (int32, error) {
	return dns.Select(context.TODO(), args.ID)
}

func (r *MutationResolver) CreateRouting(args *struct {
	Name    *string
	Routing *string
}) (c *routing.Resolver, err error) {
	var strRouting, strName string
	if args.Routing != nil {
		strRouting = *args.Routing
	}
	if args.Name != nil {
		strName = *args.Name
	}
	return routing.Create(context.TODO(), strName, strRouting)
}

func (r *MutationResolver) UpdateRouting(args *struct {
	ID      graphql.ID
	Routing string
}) (*routing.Resolver, error) {
	return routing.Update(context.TODO(), args.ID, args.Routing)
}

func (r *MutationResolver) RenameRouting(args *struct {
	ID   graphql.ID
	Name string
}) (int32, error) {
	return routing.Rename(context.TODO(), args.ID, args.Name)
}

func (r *MutationResolver) RemoveRouting(args *struct {
	ID graphql.ID
}) (int32, error) {
	return routing.Remove(context.TODO(), args.ID)
}

func (r *MutationResolver) SelectRouting(args *struct {
	ID graphql.ID
}) (int32, error) {
	return routing.Select(context.TODO(), args.ID)
}

func (r *MutationResolver) ImportNodes(args *struct {
	RollbackError bool
	Args          []*internal.ImportArgument
}) ([]*node.ImportResult, error) {
	tx := db.BeginTx(context.TODO())
	result, err := node.Import(tx, args.RollbackError, nil, args.Args)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	tx.Commit()
	return result, nil
}

func (r *MutationResolver) UpdateNode(args *struct {
	ID      graphql.ID
	NewLink string
}) (*node.Resolver, error) {
	tx := db.BeginTx(context.TODO())
	result, err := node.Update(tx, args.ID, args.NewLink)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	tx.Commit()
	return result, nil
}

func (r *MutationResolver) RemoveNodes(args *struct {
	IDs []graphql.ID
}) (int32, error) {
	return node.Remove(context.TODO(), args.IDs)
}

func (r *MutationResolver) TagNode(args *struct {
	ID  graphql.ID
	Tag string
}) (int32, error) {
	return node.Tag(context.TODO(), args.ID, args.Tag)
}

func (r *MutationResolver) ImportSubscription(args *struct {
	RollbackError bool
	Arg           internal.ImportArgument
}) (*subscription.ImportResult, error) {
	tx := db.BeginTx(context.TODO())
	result, err := subscription.Import(tx, args.RollbackError, &args.Arg)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	tx.Commit()
	ctx, caceel := context.WithTimeout(context.Background(), 10*time.Second)
	subscription.UpdateAll(ctx)
	defer caceel()
	return result, nil
}

func (r *MutationResolver) UpdateSubscription(args *struct {
	ID graphql.ID
}) (*subscription.Resolver, error) {
	return subscription.Update(context.TODO(), args.ID)
}

func (r *MutationResolver) RemoveSubscriptions(args *struct {
	IDs []graphql.ID
}) (int32, error) {
	return subscription.Remove(context.TODO(), args.IDs)
}

func (r *MutationResolver) TagSubscription(args *struct {
	ID  graphql.ID
	Tag string
}) (int32, error) {
	return subscription.Tag(context.TODO(), args.ID, args.Tag)
}

func (r *MutationResolver) CreateGroup(args *struct {
	Name         string
	Policy       string
	PolicyParams *[]struct {
		Key *string
		Val string
	}
}) (*group.Resolver, error) {
	var policyParams []config_parser.Param
	if args.PolicyParams != nil {
		// Convert.
		var params []config_parser.Param
		for _, p := range *args.PolicyParams {
			var k string
			if p.Key != nil {
				k = *p.Key
			}
			params = append(params, config_parser.Param{
				Key: k,
				Val: p.Val,
			})
		}
		policyParams = params
	}
	return group.Create(context.TODO(), args.Name, args.Policy, policyParams)
}

func (r *MutationResolver) GroupSetPolicy(args *struct {
	ID           graphql.ID
	Policy       string
	PolicyParams *[]struct {
		Key *string
		Val string
	}
}) (int32, error) {
	var policyParams []config_parser.Param
	if args.PolicyParams != nil {
		// Convert.
		var params []config_parser.Param
		for _, p := range *args.PolicyParams {
			var k string
			if p.Key != nil {
				k = *p.Key
			}
			params = append(params, config_parser.Param{
				Key: k,
				Val: p.Val,
			})
		}
		policyParams = params
	}
	return group.SetPolicy(context.TODO(), args.ID, args.Policy, policyParams)
}

func (r *MutationResolver) RemoveGroup(args *struct {
	ID graphql.ID
}) (int32, error) {
	return group.Remove(context.TODO(), args.ID)
}

func (r *MutationResolver) RenameGroup(args *struct {
	ID   graphql.ID
	Name string
}) (int32, error) {
	return group.Rename(context.TODO(), args.ID, args.Name)
}

func (r *MutationResolver) GroupAddSubscriptions(args *struct {
	ID              graphql.ID
	SubscriptionIDs []graphql.ID
}) (int32, error) {
	return group.AddSubscriptions(context.TODO(), args.ID, args.SubscriptionIDs)
}

func (r *MutationResolver) GroupDelSubscriptions(args *struct {
	ID              graphql.ID
	SubscriptionIDs []graphql.ID
}) (int32, error) {
	return group.DelSubscriptions(context.TODO(), args.ID, args.SubscriptionIDs)
}

func (r *MutationResolver) GroupAddNodes(args *struct {
	ID      graphql.ID
	NodeIDs []graphql.ID
}) (int32, error) {
	return group.AddNodes(context.TODO(), args.ID, args.NodeIDs)
}

func (r *MutationResolver) GroupDelNodes(args *struct {
	ID      graphql.ID
	NodeIDs []graphql.ID
}) (int32, error) {
	return group.DelNodes(context.TODO(), args.ID, args.NodeIDs)
}
