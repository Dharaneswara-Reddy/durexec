# Local hardware baseline

Measured 2026-10-28 with read-only commands. No benchmark was run and nothing
was reconfigured.

---

## Verified specification

| Property | Value | How obtained |
|---|---|---|
| CPU | **13th Gen Intel Core i5-1335U** | `lscpu` |
| Architecture | x86_64 | `lscpu` |
| **Physical cores** | **10** (2 performance + 8 efficiency) | `lscpu -p=CORE \| sort -u` |
| **Logical threads** | **12** | `nproc` |
| Clock range | 400 MHz – 4600 MHz | `lscpu` |
| L3 cache | 12 MiB | `lscpu` |
| AVX-512 | **No** (disabled on hybrid parts) | `/proc/cpuinfo` |
| **Total RAM** | **15 GiB** | `free -h` |
| Available RAM | 6.1 GiB | `free -h` |
| **Swap in use** | **3.3 GiB of 8.0 GiB** | `free -h` |
| Disk | 451 GB btrfs, **65 GB free (86 % used)** | `df -hT` |
| Disk type | NVMe SSD (non-rotational) | `lsblk` |
| OS | Fedora Linux 42, kernel 6.17.5 | `uname -srm` |
| GPU | Intel integrated; **no NVIDIA, no CUDA** | no `nvidia-smi` present |

The previously reported "6.4 GB available" was free memory at that moment, not
capacity. The machine has **15 GiB total**.

---

## Container and VM environment

| Property | Value |
|---|---|
| Docker | 28.5.1, **running natively on the host** (not inside a VM) |
| Storage driver | overlay2 |
| cgroup | v2, systemd driver |
| Resources Docker sees | 12 CPUs, 15.3 GiB — **no artificial limit** |
| Containers running now | 2 (`oj-rabbitmq`, `oj-redis`) — unrelated to durexec |
| minikube | kvm2 driver, **2 vCPU / 3900 MB / 20 GB**, currently **Stopped** |

Docker being native matters: on Linux there is no Docker Desktop VM taking a
fixed slice, so a container can reach all 12 threads. Starting minikube would
take 2 vCPU and 3.9 GB back immediately.

---

## Is the development environment already constrained?

**Yes on memory. No on CPU.**

- **3.3 GiB of swap is in use** while 6.1 GiB of RAM is available. Swap being
  touched at all means the working set exceeded physical memory at some point
  this session. The `host-is-ram-constrained` note records VS Code crashing
  twice on 2026-10-27 for this reason.
- **Disk is 86 % full** with 65 GB free. Enough for container images, but
  btrfs degrades as it approaches full, and the event log has no garbage
  collection.
- **CPU is not the constraint.** Twelve threads are rarely all busy; the
  durexec lab saturated on the *database*, not on available cores.

---

## Honest caveat on the CPU

The i5-1335U is a **15 W U-series laptop part with a hybrid topology**. Three
consequences that a core count alone hides:

1. **Only 2 of the 10 cores are performance cores.** The other 8 are
   efficiency cores with materially lower per-thread throughput. A thread that
   lands on an E-core measures something different from one on a P-core, and
   the kernel decides which.
2. **Sustained load is thermally limited.** A 15 W part boosts to 4.6 GHz
   briefly, then throttles. Any run longer than about a minute measures the
   cooling system as much as the processor.
3. **The machine is shared** with a desktop session, a browser, an editor and
   two unrelated containers. The Phase 6 sweep already records background load
   beside each result for exactly this reason.

**This is why absolute throughput measured here is not publishable**, and why
`docs/RESULTS_SUMMARY.md` reports scaling *ratios* and correctness *counts*
instead.

---

## What this means for the cloud comparison

The laptop is **not weak**. With 10 physical cores and 15 GiB it has more of
both than every EC2 instance affordable at this budget — `t3.medium` has
**one** physical core and 4 GiB.

**The reason to go to the cloud is therefore not more compute.** It is
**failure-domain separation**: the ability to destroy one machine and watch
the others survive, which no single laptop can provide at any specification.
That argument is developed in `TEST-ARCHITECTURE-OPTIONS.md`.

---

## Unknowns

| Metric | Status | Command to obtain it |
|---|---|---|
| Disk IOPS and latency | **Unknown** — no benchmark run | `fio --name=r --rw=randread --bs=4k --size=512M --runtime=30 --time_based` |
| Sustained all-core clock under load | **Unknown** | `stress-ng --cpu 12 --timeout 60s` with `watch -n1 'grep MHz /proc/cpuinfo'` |
| Memory bandwidth | **Unknown** | `mbw 1024` |

None was run, because each would load a machine that is already swapping.

---

## Commands to reproduce

```sh
lscpu | grep -E '^(Model name|Architecture|CPU\(s\):|Thread|Core|Socket|L3)'
lscpu -p=CORE | grep -v '^#' | sort -u | wc -l     # physical cores
nproc                                              # logical threads
grep -qo avx512f /proc/cpuinfo && echo yes || echo no
free -h
df -hT / /home
lsblk -d -o NAME,ROTA,SIZE,MODEL
docker info | grep -E 'Server Version|Storage Driver|Cgroup|CPUs|Total Memory'
python3 -c "import json;d=json.load(open('$HOME/.minikube/machines/minikube/config.json'))['Driver'];print(d['CPU'],d['Memory'],d['DiskSize'])"
```
