package gate

import "strings"

var blockedExact = map[string]struct{}{
	"pg_terminate_backend": {}, "pg_cancel_backend": {}, "pg_reload_conf": {},
	"pg_rotate_logfile": {}, "pg_switch_wal": {}, "pg_promote": {},
	"pg_log_backend_memory_contexts": {}, "pg_log_standby_snapshot": {},
	"set_config": {}, "pg_sleep": {}, "pg_sleep_for": {}, "pg_sleep_until": {},
	"pg_read_file": {}, "pg_read_binary_file": {}, "pg_stat_file": {},
	"pg_notify": {}, "nextval": {}, "setval": {},
	"pg_backup_start": {}, "pg_backup_stop": {},
	"pg_import_system_collations": {}, "pg_sync_replication_slots": {},
}

var blockedPrefixes = []string{
	"pg_advisory_", "pg_try_advisory_", "pg_ls_", "lo_", "dblink",
	"pg_logical_", "pg_create_", "pg_drop_", "pg_replication_origin_",
	"pg_stat_reset", "pg_wal_replay_", "pg_file_",
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
