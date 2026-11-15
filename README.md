# lansync

Keep a LAN's address plan in a file and make the network controller follow it.

You declare how each network's address space is divided — which part is the DHCP
pool, which part is for switches and access points, which part belongs to
devices that set their own address — and lansync compares that with the
controller and, where you allow it, changes the controller to match.

```
declaration ──▶ check ──▶ provider reads ──▶ diff ──▶ provider writes ──▶ report
```

The declaration and the comparison do not depend on any vendor. A **provider**
reads and writes one kind of controller. This repository has a provider for
**UniFi Network**.

## What it manages today

| | UniFi |
|---|---|
| DHCP pool (start–stop) | ✅ |
| DHCP lease time | ✅ |
| Reservations (MAC → IP) | declared and validated; not applied yet |

lansync only touches what you declare. Networks, fields and reservations that are
not in the file are read at most, never changed.

## The declaration

```yaml
layouts:
  office:
    blocks:
      - { name: gateway, ranges: [.1] }
      - { name: infra,   ranges: [.10-.49] }
      - { name: devices, ranges: [.50-.59, .70-.79] }
      - { name: dhcp,    ranges: [.100-.199] }
      - { name: static,  ranges: [.200-.254] }

networks:
  - name: floor-2
    match: { vlan: 2 }
    subnet: 192.0.2.0/24
    layout: office
    dhcp: { block: dhcp, leaseTime: 8h }

reservations:
  - { name: ap-01, mac: "00:00:5e:00:53:01", network: floor-2, block: infra, ip: .11 }
```

A fuller example is in [`examples/office.yaml`](examples/office.yaml).

### Addresses

- `.N` is the N-th address of the network's subnet, counted from the network
  address. In `192.0.2.0/24`, `.11` is `192.0.2.11`; in `192.0.2.0/23`, `.300` is
  `192.0.3.44`. This is what lets one layout serve many networks.
- A full address (`192.0.2.11`) works too and must be inside the subnet.
- A range is `A-B` (inclusive) or a single address `A`.
- Network and broadcast addresses are refused.

### `layouts`

Named address plans. A layout is a list of **blocks**; a block has a `name` and
one or more `ranges`. Ranges of a block need not be contiguous.

### `networks`

| Field | |
|---|---|
| `name` | Your name for the network. Unique. |
| `match` | How the provider finds the network on the controller. Keys are provider specific (UniFi: `vlan`, `name`, `id`). Every entry must match, and exactly one network must match. |
| `subnet` | The network's IPv4 subnet. It must equal what the controller reports, or nothing is done for that network. |
| `layout` | Optional. Its blocks become the network's blocks. |
| `blocks` | Optional. More blocks, added to the layout's. |
| `dhcp.block` | The block that is the DHCP pool. |
| `dhcp.leaseTime` | Optional, e.g. `8h`. Left alone when omitted. |

The declaration says what a network should be, never whether to change it now.
That is decided when lansync is run (`apply -network`, below).

Common values can be shared with YAML anchors, e.g.
`dhcp: &wired { block: dhcp, leaseTime: 8h }` on one network and `dhcp: *wired` on
the next. Merge keys (`<<:`) that override fields are not supported.

Rules checked before anything is read from the controller:

- No address belongs to two blocks. That is what keeps the DHCP pool out of the
  addresses that are set on devices: give those their own block.
- `dhcp.block` names a block of the network.

There is no field that says what a block is "for". A block becomes the DHCP pool
because `dhcp.block` points at it; a reservation is checked against a block
because it names it. Use YAML comments for notes.

### `reservations`

| Field | |
|---|---|
| `name` | Unique. |
| `mac` | 48-bit MAC address. |
| `network` | A declared network. |
| `block` | Optional. When set, `ip` must be in that block. |
| `ip` | Address as above. Must be outside the DHCP pool. |

## Commands

```sh
lansync check -f lansync.yaml
lansync plan  -f lansync.yaml -provider unifi -unifi-url https://192.0.2.1/proxy/network/integration
lansync apply -f lansync.yaml -network floor-2 -provider unifi -unifi-url https://192.0.2.1/proxy/network/integration
```

- `check` validates the file and does not contact anything.
- `plan` shows what differs and changes nothing.
- `apply -network NAMES` changes the named networks (comma-separated, or `all`)
  and reports the others as `plan` does. A name that is not declared, or a
  declaration that needs something the provider cannot do (e.g. two ranges in
  one UniFi pool), applies nothing at all.

Output is a table, or with `-o json` one JSON object per network:

```json
{"provider":"unifi","network":"floor-2","selector":"vlan=2","mode":"apply","id":"…","status":"applied","changes":[{"field":"dhcp.ranges","from":"192.0.2.6-192.0.2.254","to":"192.0.2.100-192.0.2.199"}]}
```

`mode` is `apply` for the networks named in `-network`, otherwise `dry-run`.
`status` is `in-sync`, `drift` (differs, left as is), `applied` or `failed`. A
failure before any network is looked at (invalid declaration, bad flags) is one
line with `status: failed` and no `network`.

Exit code: `1` if anything failed, otherwise `0`. With `-detailed-exitcode`, `2`
when a network differs and was left as is.

## UniFi

Uses the official **Integration API** of UniFi Network 10.x.

| Flag | |
|---|---|
| `-unifi-url` | API root without `/v1`. UniFi OS consoles: `https://<console>/proxy/network/integration`. |
| `-unifi-legacy-url` | Root of the controller's own API, read to verify updates. Default: `-unifi-url` without `/integration`. |
| `-unifi-site` | Site internal reference (default `default`), name or ID. |
| `-unifi-api-key-file` | File with the API key. Otherwise `$LANSYNC_UNIFI_API_KEY`. |
| `-unifi-ca` | PEM of the certificate to trust. A console's self-signed certificate can be given directly. |
| `-unifi-tls-server-name` | Name to check the certificate against when the URL uses an IP. |

`plan` needs read access. `apply` needs a key whose admin may change network
settings; create a dedicated admin for it.

**How an update is made safe.** The API replaces the whole network on update —
a field left out is reset (a missing DHCP configuration means "no DHCP"). So
lansync reads the network, changes only the DHCP pool and lease time, writes it
back, reads it again, and fails loudly if anything else differs.

The Integration API does not show every setting of a network (mDNS, IGMP
snooping, DHCP DNS options, …). So lansync also reads the network's full record
from the controller's own API (`/api/s/{site}/rest/networkconf`, matched by
`external_id`) before and after the update, and fails — naming each setting with
its old and new value — if anything but the pool and lease time changed. That
API is undocumented and only read.

It never turns a DHCP server on or off, and refuses networks that relay DHCP.

UniFi has one range per DHCP pool, so a `dhcp.block` with several ranges is
refused for UniFi.

## Writing a provider

Implement [`provider.Provider`](provider/provider.go):

```go
type Provider interface {
    Name() string
    Capabilities() Capabilities
    FindNetworks(ctx context.Context, selector map[string]string) ([]Network, error)
    UpdateNetwork(ctx context.Context, u NetworkUpdate) error
}
```

`Capabilities` declares the `match` keys and what can be managed; the planner
refuses declarations that need more. `UpdateNetwork` must change only the fields
set in the update. Validation, diffing, modes and reporting are shared, in
packages [`spec`](spec) and [`plan`](plan).

## Image

`ghcr.io/lesomnus/lansync:edge` (amd64, arm64) is built from `main`.

## License

Apache-2.0
