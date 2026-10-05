# Agent error codes

The agent logs every coded error with a `code` field and counts it in `pgha_errors_total{code="..."}`. Quote the code when you report a problem.

| Code | Area | Cause |
| --- | --- | --- |
| 1101 | config | the agent's environment is missing a setting or holds an invalid one |
| 1102 | bootstrap | the data directory belongs to a different cluster than the one recorded on the Lease; the member refuses to start |
| 1103 | upgrade | the data directory was created by a different PostgreSQL major version than the server image |
| 1104 | tls | the peer certificate, key or CA bundle is missing or invalid; the agent does not run without mutual TLS |
| 1105 | lease | the cluster Lease could not be read or written |
| 1106 | failover | the standby holding the Lease did not finish promotion in time; the Lease was released |
| 1107 | rejoin | pg_rewind could not resynchronise a former primary; its data was moved aside and it is re-cloned |
| 1108 | rejoin | cloning a standby from the primary failed; it is retried |
| 1109 | bootstrap | creating or restoring the cluster's first data failed |
| 1110 | backup | a WAL-G base backup or its retention run failed |
| 1111 | fencing | the primary could not renew the Lease in time and stopped PostgreSQL so it cannot accept writes |
| 1112 | roles | creating or updating roles and databases on the primary failed; it is retried |
| 1113 | restore | archive recovery ran out of WAL before the restore target; the member does not restart it, since the same WAL gives the same result |

The table is generated from `internal/errs`; a unit test fails when the two drift apart.
