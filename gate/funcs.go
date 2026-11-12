package gate

import "strings"

// The blocklist below was reviewed against every volatile pg_catalog
// function of PostgreSQL 18 (pg_proc where pronamespace = pg_catalog and
// provolatile = 'v'), on 2026-11-12.
//
// Blocked from that scan, in addition to the original spec entries:
//   - run SQL text given as an argument: ts_stat, ts_rewrite
//   - change physical index state: brin_* (summarize / desummarize),
//     gin_clean_pending_list
//   - change planner statistics: pg_restore_*, pg_clear_*
//   - change stats-system state: pg_stat_clear_snapshot,
//     pg_stat_force_next_flush (pg_stat_reset* was already blocked)
//   - write catalogs or server state: binary_upgrade_*, pg_nextoid,
//     pg_stop_making_pinned_objects, pg_extension_config_dump
//   - large-object access: loread, lowrite (lo_* was already blocked)
//   - write a file: pg_export_snapshot (writes to pg_snapshots/)
//   - read server files: pg_show_all_file_settings, pg_hba_file_rules,
//     pg_ident_file_mappings, pg_current_logfile,
//     pg_available_wal_summaries, pg_wal_summary_contents
//   - change session state: setseed
//   - language internals: plpgsql_* (handlers and the validator, which
//     compiles function bodies)
//
// Judged safe (read-only, or not callable from SQL):
//   - randomness and clocks: random, random_normal, array_sample,
//     array_shuffle, gen_random_uuid, uuidv4, uuidv7, clock_timestamp,
//     timeofday
//   - read server status and statistics: pg_stat_get_*, pg_stat_have_stats,
//     pg_lock_status, pg_blocking_pids, pg_safe_snapshot_blocking_pids,
//     pg_isolation_test_session_is_blocked, pg_control_*, pg_current_wal_*,
//     pg_last_wal_*, pg_last_xact_replay_timestamp, pg_is_in_recovery,
//     pg_is_wal_replay_paused, pg_get_wal_replay_pause_state,
//     pg_get_wal_summarizer_state, pg_get_wal_resource_managers, pg_get_aios,
//     pg_get_wait_events, pg_get_backend_memory_contexts,
//     pg_get_shmem_allocations*, pg_get_loaded_modules, pg_jit_available,
//     pg_notification_queue_usage, pg_show_replication_origin_status,
//     pg_get_multixact_members, pg_last_committed_xact, pg_prepared_xact,
//     pg_xact_commit_timestamp*, pg_xact_status, txid_status, current_query
//   - read sizes and catalog data: pg_*_size, pg_partition_tree,
//     pg_partition_ancestors, pg_collation_actual_version,
//     pg_database_collation_actual_version, pg_sequence_last_value,
//     pg_get_sequence_data, currval, lastval, currtid2, amvalidate
//   - trigger functions (error unless fired by a trigger): RI_FKey_*,
//     tsvector_update_trigger*, suppress_redundant_updates_trigger,
//     unique_key_recheck
//   - take an internal argument, so SQL cannot call them: *handler
//     (bthandler, heap_tableam_handler, ...), system, bernoulli,
//     dsnowball_init, dsnowball_lexize
var blockedExact = map[string]struct{}{
	"pg_terminate_backend": {}, "pg_cancel_backend": {}, "pg_reload_conf": {},
	"pg_rotate_logfile": {}, "pg_switch_wal": {}, "pg_promote": {},
	"pg_log_backend_memory_contexts": {}, "pg_log_standby_snapshot": {},
	"set_config": {}, "pg_sleep": {}, "pg_sleep_for": {}, "pg_sleep_until": {},
	"pg_read_file": {}, "pg_read_binary_file": {}, "pg_stat_file": {},
	"pg_notify": {}, "nextval": {}, "setval": {},
	"pg_backup_start": {}, "pg_backup_stop": {},
	"pg_import_system_collations": {}, "pg_sync_replication_slots": {},
	"ts_stat": {}, "ts_rewrite": {}, "gin_clean_pending_list": {},
	"pg_stat_clear_snapshot": {}, "pg_stat_force_next_flush": {},
	"pg_nextoid": {}, "pg_stop_making_pinned_objects": {},
	"pg_extension_config_dump": {}, "loread": {}, "lowrite": {},
	"pg_export_snapshot": {}, "pg_show_all_file_settings": {},
	"pg_hba_file_rules": {}, "pg_ident_file_mappings": {},
	"pg_current_logfile": {}, "pg_available_wal_summaries": {},
	"pg_wal_summary_contents": {}, "setseed": {},
}

var blockedPrefixes = []string{
	"pg_advisory_", "pg_try_advisory_", "pg_ls_", "lo_", "dblink",
	"pg_logical_", "pg_create_", "pg_drop_", "pg_replication_origin_",
	"pg_stat_reset", "pg_wal_replay_", "pg_file_",
	"brin_", "pg_restore_", "pg_clear_", "binary_upgrade_", "plpgsql_",
}

var blockedSuffixes = []string{"_to_xml", "_to_xmlschema", "_to_xml_and_xmlschema"}

// Blocked reports whether a function name is on the blocklist: functions
// with side effects, file or network access, or that run arbitrary SQL.
// The name is compared lower-cased.
func Blocked(name string) bool {
	name = strings.ToLower(name)
	if _, ok := blockedExact[name]; ok {
		return true
	}
	for _, p := range blockedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, s := range blockedSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return strings.Contains(name, "replication_slot")
}
