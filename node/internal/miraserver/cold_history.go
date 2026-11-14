package miraserver

import (
	"context"

	"github.com/ssine/mira/node/internal/miraserver/coldstore"
)

// configureColdHistory installs the process-wide reader for frozen history
// payloads. Without a bucket, frozen rows fail explicitly instead of being
// returned as placeholders.
func configureColdHistory(configuration Config) (coldstore.ObjectStore, error) {
	cold := configuration.Foundation.ColdStorage
	if !cold.Enabled() {
		coldstore.SetDefault(nil)
		return nil, nil
	}
	objects, err := coldstore.NewS3(coldstore.ObjectConfig{
		Endpoint: cold.Endpoint, Region: cold.Region, Bucket: cold.Bucket, Prefix: cold.Prefix,
		AccessKeyID: cold.AccessKeyID, SecretAccessKey: cold.SecretAccessKey, PathStyle: cold.PathStyle,
		TCPCongestion: cold.TCPCongestion,
	}, nil)
	if err != nil {
		return nil, err
	}
	store, err := coldstore.New(objects, coldstore.Options{CacheDir: cold.CacheDir, CacheBytes: cold.CacheBytes})
	if err != nil {
		return nil, err
	}
	coldstore.SetDefault(store)
	return objects, nil
}

// prefetchColdHistory downloads every frozen payload of one history read
// before it starts, with bounded concurrency.
func prefetchColdHistory(ctx context.Context, query dbtx, storeID, threadID string, generation any, version int64) error {
	rows, err := query.Query(ctx, `SELECT cold_sha256 FROM codex_thread_events_versioned WHERE store_id=$1 AND thread_id=$2 AND generation=$3
 AND store_event_seq<=$4 AND cold_sha256 IS NOT NULL`, storeID, threadID, generation, version)
	if err != nil {
		return err
	}
	shas := []string{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return err
		}
		shas = append(shas, sha)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return coldstore.Prefetch(ctx, shas)
}

// prefetchClaudeEntries downloads the frozen bodies of one native transcript
// suffix before it is streamed.
func prefetchClaudeEntries(ctx context.Context, query dbtx, sessionID, subpath string, after int64) error {
	rows, err := query.Query(ctx, `SELECT cold_sha256 FROM mira_claude_entries WHERE session_id=$1 AND subpath=$2 AND seq>$3 AND cold_sha256 IS NOT NULL`, sessionID, subpath, after)
	if err != nil {
		return err
	}
	shas := []string{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return err
		}
		shas = append(shas, sha)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return coldstore.Prefetch(ctx, shas)
}
