# galera operator experiments

This repo is a playground for managing a galera cluster in kubernetes.

# How to run the operator

```
# create a namespace and instantiate all the example resources
$ oc new-project galera-operator-example

# Instantiate associated resources: config, and helper scripts
$ oc apply -f config/samples/galera-config.yaml
$ oc apply -f config/rbac/service_account.yaml

# Instantiate default passwords for the database
$ oc apply -f config/samples/galera-secrets.yaml

# start the controller
$ make install run

# Instantiate a 3-node galera custom resource
$ oc apply -f config/samples/database_v1_galera.yaml

# The galera operator will create a statefulset automatically
$ oc get pods
NAME       READY   STATUS    RESTARTS   AGE
galera-0   1/1     Running   0          17m
galera-1   1/1     Running   0          17m
galera-2   1/1     Running   0          16m

$ oc rsh -c galera galera-1 ps -ef
UID          PID    PPID  C STIME TTY          TIME CMD
root           1       0  0 14:26 ?        00:00:00 /bin/sh /usr/bin/mysqld_safe --wsrep-cluster-address=gcomm://galera-0.galera,galera-1.galera,galera-2.galera
mysql        663       1  0 14:27 ?        00:00:01 /usr/libexec/mysqld --basedir=/usr --datadir=/var/lib/mysql --plugin-dir=/usr/lib64/mariadb/plugin --user=mysql --wsrep_on=ON --wsrep_provider=/usr/lib64/galera/libgalera_smm.so --wsrep-cluster-address=gcomm://galera-0.galera,galera-1.g
root         707       0  0 14:44 pts/0    00:00:00 ps -ef

$ oc get galera/galera -o yaml
apiVersion: database.example.com/v1
kind: Galera
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"database.example.com/v1","kind":"Galera","metadata":{"annotations":{},"name":"galera","namespace":"galera-operator-example"},"spec":{"image":"quay.io/tripleomaster/openstack-mariadb:current-tripleo","secret":"galera-secrets","size":3}}
  creationTimestamp: "2022-07-28T14:26:43Z"
  generation: 1
  name: galera
  namespace: galera-operator-example
  resourceVersion: "1027004"
  uid: 7346f4a9-f5ab-412e-a2e8-bc97a241db4a
spec:
  image: quay.io/tripleomaster/openstack-mariadb:current-tripleo
  secret: galera-secrets
  size: 3
status:
  attributes:
    galera-0:
      gcomm: gcomm://galera-0.galera,galera-1.galera,galera-2.galera
      seqno: "0"
    galera-1:
      gcomm: gcomm://galera-0.galera,galera-1.galera,galera-2.galera
      seqno: "0"
    galera-2:
      gcomm: gcomm://
      seqno: "0"
  bootstrapped: true
```