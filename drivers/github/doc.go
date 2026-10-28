// Package github is the tobira driver for GitHub: a repository's issues,
// pull requests, comments, commits, branches, tags, releases, labels,
// milestones and workflow runs as collections, read and edited through the
// GitHub REST API, with the GraphQL API as an escape hatch.
//
// It registers the "github" driver for the github:// scheme. Queries are
// one JSON object (see parseRequest); the driver has no SDK dependency.
package github
