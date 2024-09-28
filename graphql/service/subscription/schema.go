/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2023, daeuniverse Organization <team@v2raya.org>
 */

package subscription

func Schema() (string, error) {
	return `
type Subscription {
	id: ID!
	updatedAt: Time!
	tag: String
	link: String!
	cronExp: String!
	cronEnable: Boolean!
	status: String!
	info: String!
	nodes(first: Int, after: ID): NodesConnection!
}
`, nil
}
