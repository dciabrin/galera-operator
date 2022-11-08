#!/bin/bash

# Note: it seems like we can't use startup probe concurrently for a statefulset
# or a new replica won't be spawn until the startup probe returns OK for the previous one
# so instead: only probe once the mysql server started locally.
if test \! -e /var/lib/mysql/mysql.sock; then
    exit 0
fi

# This secret is mounted by k8s and always up to date
read -s -u 3 3< /var/lib/secrets/dbpassword MYSQL_PWD
export MYSQL_PWD

case "$1" in
    [rR]eadiness)
	mysql -NEe "show status like 'wsrep_local_state_comment';" | tail -1 | grep -q -w -e Synced;;
    [lL]iveness)
	mysql -NEe "show status like 'wsrep_cluster_status';" | tail -1 | grep -q -w -e Primary;;
    *)
	echo "Invalid probe option '$1'"
	exit 1;;
esac
