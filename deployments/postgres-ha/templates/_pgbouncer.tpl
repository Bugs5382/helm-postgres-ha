{{/* pgbouncer.ini, rendered once for the ConfigMap and its checksum. */}}
{{- define "pgha.pgbouncerIni" -}}
[databases]
* = host={{ printf "%s.%s.svc" (include "pgha.primary" .) .Release.Namespace }} port=5432

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
unix_socket_dir =
; Every app role logs in with its own SCRAM credentials, looked up on the
; primary through a SECURITY DEFINER function the agent maintains.
auth_type = scram-sha-256
auth_file = /run/pgbouncer/userlist.txt
auth_user = pgbouncer_auth
auth_dbname = postgres
auth_query = SELECT usename, passwd FROM pgbouncer.get_auth($1)
; The new primary logs in here after a promotion to KILL and RESUME each
; database, dropping connections to the old primary.
admin_users = pgbouncer_auth
pool_mode = {{ .Values.pgbouncer.poolMode }}
default_pool_size = {{ .Values.pgbouncer.defaultPoolSize }}
max_client_conn = {{ .Values.pgbouncer.maxClientConn }}
ignore_startup_parameters = extra_float_digits
; TLS both ways: clients must use it, and the server is verified against
; the cluster CA by its primary Service name.
client_tls_sslmode = require
client_tls_cert_file = /etc/pgbouncer/tls/tls.crt
client_tls_key_file = /etc/pgbouncer/tls/tls.key
server_tls_sslmode = verify-full
server_tls_ca_file = /etc/pgbouncer/tls/ca.crt
; Fail fast after a failover: drop connections to a server that went away
; and never hand out one that no longer answers.
server_check_query = select 1
server_check_delay = 5
server_connect_timeout = 5
server_login_retry = 1
server_fast_close = 1
server_lifetime = 1800
tcp_keepalive = 1
tcp_keepidle = 10
tcp_keepintvl = 5
tcp_keepcnt = 3
; Keepalives only probe idle sockets. A query sent to a primary that froze
; is never acknowledged, so without this the connection (and every client
; queued behind it) waits for the kernel's retransmission timeout.
tcp_user_timeout = 10000
log_connections = 0
log_disconnections = 0
{{- end -}}
