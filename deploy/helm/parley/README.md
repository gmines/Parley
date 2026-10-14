# Parley Helm chart

Deploys the whole Parley server: `api`, `realtime`, the voice stack
(voice-control, Kamailio, FreeSWITCH), Postgres and NATS.

```
                       ┌──────────── Ingress (Traefik on k3s), one hostname ───────────┐
 app ── HTTPS/WSS 443 ─┤  /v1/realtime → realtime   /v1/calls → voice-control   / → api │
                       └────────────────────────────────────────────────────────────────┘
 app ── SIP TLS 5061 ─────────► media node (hostNetwork): Kamailio → FreeSWITCH ◄── voice-control
 app ── audio UDP 10000-10999 ─► media node: FreeSWITCH            (all on the node's loopback)
                                 api, realtime, voice-control → Postgres, NATS (cluster services)
```

The voice pod runs on the node's own network. SIP and audio need an address
clients can reach directly, with no load balancer or NAT in between: that's
what prevents one-way audio. So it runs on one node you choose (the
**media node**), and clients send audio straight to it.

Call setup is SIP over TLS on port 5061 to `host`, using the same certificate
as the web ingress: clients check it, so the audio encryption keys (in the
call setup) can't be read or changed on the way. Kamailio reloads the
certificate when cert-manager renews it, without dropping calls. Point
`host` straight at your server: a CDN or proxy in front (like Cloudflare's
orange cloud) only carries web traffic, not SIP or audio.

## Home lab: k3s on a few VMs

### 1. VMs

| VM | Role | Size (to start) |
|---|---|---|
| `k3s-server` | control plane (can also run workloads) | 2 vCPU, 4 GB |
| `k3s-worker-1` | api, realtime, Postgres, NATS | 2 vCPU, 4 GB, 20 GB disk |
| `k3s-media` | voice pod only | 2 vCPU, 2 GB |

One VM is enough for testing: label it as the media node too. Give
`k3s-media` a fixed IP (DHCP reservation or static).

### 2. k3s

```sh
# on k3s-server
curl -sfL https://get.k3s.io | sh -
sudo cat /var/lib/rancher/k3s/server/node-token

# on each worker
curl -sfL https://get.k3s.io | K3S_URL=https://<k3s-server-ip>:6443 K3S_TOKEN=<token> sh -

# your workstation: copy /etc/rancher/k3s/k3s.yaml from the server to
# ~/.kube/config and replace 127.0.0.1 with the server's IP
kubectl get nodes
```

k3s includes what the chart expects: Traefik (Ingress), ServiceLB (exposes
Traefik on the nodes' ports 80/443) and local-path storage (Postgres volume).

### 3. The media node

```sh
kubectl label node k3s-media parley.chat/media=true
# Optional: keep everything else off it
kubectl taint node k3s-media parley.chat/media=true:NoSchedule
```

If you add the taint, also set this in your values:

```yaml
voice:
  tolerations:
    - { key: parley.chat/media, operator: Equal, value: "true", effect: NoSchedule }
```

Open the voice ports on the media VM if it runs a firewall:

```sh
sudo firewall-cmd --permanent --add-port=5061/tcp --add-port=10000-10999/udp && sudo firewall-cmd --reload
```

### 4. DNS and TLS

Pick a hostname (say `parley.home.example.org`) and point it at a node's IP
(Traefik answers on every node). Then choose one of:

- **Real certificate (recommended):** install cert-manager with a Let's Encrypt
  DNS-01 issuer (works without exposing port 80) and set
  `ingress.annotations: {cert-manager.io/cluster-issuer: <issuer>}`.
- **LAN testing:** `ingress.tls.enabled: false`. Logins then travel
  unencrypted, so only use it on a trusted network. The app accepts
  `http://` server addresses.

A self-signed certificate won't work unless every client's OS trusts it (the
app checks certificates against the system's trust store).

### 5. Install

Generate the access-token key pair (keep it safe: it signs every login):

```sh
podman run --rm ghcr.io/gmines/parley-api:0.1.0 keygen
```

`my-values.yaml`:

```yaml
host: parley.home.example.org
secrets:
  signingKey: "<PARLEY_SIGNING_KEY from keygen>"
  verifyKey: "<PARLEY_VERIFY_KEY from keygen>"
voice:
  # The media node's LAN IP, or your public IP (see below).
  advertiseAddress: 192.168.19.60
postgres:
  backup:
    enabled: true
```

```sh
helm install parley deploy/helm/parley -n parley --create-namespace -f my-values.yaml
kubectl -n parley get pods
```

In the app, set the server to `https://parley.home.example.org`.

The SIP secret, the Event Socket password and the Postgres password are
generated on first install and kept across upgrades. Only the key pair has to
come from you.

### 6. Callers outside your network

Forward these from your router:

| Port | To |
|---|---|
| TCP 443 | any k3s node (Traefik) |
| TCP 5061 | the media node |
| UDP 10000-10999 | the media node |

Then set `voice.advertiseAddress` to your **public** IP, and point `host`
at it too (SIP over TLS goes to `host`). FreeSWITCH still binds the media
node's own address, and the router forwards to it.

LAN clients then also use the public IP, so your router must support
hairpin NAT (most do).

## Using your own Postgres

```sh
kubectl -n parley create secret generic parley-db \
  --from-literal=url='postgres://parley:<password>@db.lan:5432/parley?sslmode=require'
```

```yaml
postgres:
  enabled: false
externalDatabase:
  existingSecret: parley-db
```

Parley needs its own database (`CREATE DATABASE parley OWNER parley`): the api
creates and migrates its tables on startup. voice-control reads the same
database.

## Backups (bundled Postgres)

With `postgres.backup.enabled`, a CronJob runs `pg_dump` nightly (03:00) into
its own volume and keeps the newest 7 dumps. That volume survives
`helm uninstall`. Take one now, or restore:

```sh
kubectl -n parley create job --from=cronjob/parley-postgres-backup backup-now
kubectl -n parley exec -i parley-postgres-0 -- pg_restore --clean -U parley -d parley < parley-<date>.dump
```

The dumps sit on the worker's disk (local-path). Copy them off the cluster too
(for example with a VM-level backup of that node).

## Upgrading

```sh
helm upgrade parley deploy/helm/parley -n parley -f my-values.yaml
```

The voice pod is replaced stop-first (the host ports can't be shared), so
**calls in progress drop** during an upgrade. Clients can rejoin once the new
pod is up (a few seconds). api and realtime roll over without downtime with 2+ replicas.

## Images

`ghcr.io/gmines/parley-{api,realtime,voice-control,kamailio,freeswitch}`, built
by `.github/workflows/server-images.yml`:

- `:<version>` from `server-v<version>` tags (the chart's `appVersion` is the default)
- `:development` / `:main` and `:sha-<commit>` from branch pushes

To follow the development branch, set `image.tag: development` and `image.pullPolicy: Always`.

## Clients before 0.4

Those only speak plain SIP (TCP/UDP 5060): call setup, including the audio
keys, crosses the network unencrypted, and routers with a "SIP ALG" tend to
rewrite it. They're refused by default and told to update. To let them in
while people update, set `voice.sip.plain.enabled: true` and open TCP+UDP
5060; clients 0.4+ keep using TLS.

## Hardening built in

- Kamailio only accepts calls with short-lived credentials from voice-control
  (one room, 5 minutes), never answers registrations, and hides its version.
- More than ~30 requests in 2 seconds from one IP gets that IP ignored for 5
  minutes; known SIP scanners are ignored outright.
- FreeSWITCH and its Event Socket only listen on the node's loopback. Audio
  must be SRTP: unencrypted calls are refused.

## Not in this chart yet

- TURN relay for networks that block UDP.
- More than one media node (rooms would be assigned to nodes).
