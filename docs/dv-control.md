# DV Control Reference

This is the detailed reference for the ndn-dv routing daemon control tool.

## `ndnd dv link-create`

The link-create command creates a new neighbor link. A new permanent face will be created for the neighbor if a matching face does not exist.

```bash
# Create a UDP neighbor link
ndnd dv link-create udp://suns.cs.ucla.edu

# Create a TCP neighbor link
ndnd dv link-create tcp4://hobo.cs.arizona.edu:6363
```

## `ndnd dv link-destroy`

The link-destroy command destroys a neighbor link. The face associated with the neighbor will be destroyed.

```bash
# Destroy a neighbor link by URI
ndnd dv link-destroy udp://suns.cs.ucla.edu
```

## `ndnd dv prefix-announce`

The `prefix-announce` command injects a local entry directly into the DV prefix egress state.

```bash
# Announce /example via face 300 with cost 0
ndnd dv prefix-announce prefix=/example face=300 cost=0
```

## `ndnd dv prefix-withdraw`

The `prefix-withdraw` command removes a local prefix entry from the DV prefix egress state.

```bash
# Withdraw /example from face 300
ndnd dv prefix-withdraw prefix=/example face=300
```
