---
marp: true
theme: default
paginate: true
backgroundColor: "#0e2744"
color: "#f1f5f9"
header: "Incus | Linux Containers"
footer: "linuxcontainers.org | github.com/lxc/incus"
style: |
  section {
    background-image: url('navy-chalkboard.jpg');
    background-size: cover;
    background-position: center;
    background-color: #0c1e33;
    color: #f1f5f9;
    font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
    font-size: 24px;
    padding: 65px 60px 40px !important;
    position: relative;
    display: flex !important;
    flex-direction: column !important;
    justify-content: flex-start !important;
    align-content: flex-start !important;
    place-content: flex-start !important;
  }
  section::before {
    content: '';
    position: absolute;
    top: 22px;
    right: 35px;
    width: 48px;
    height: 48px;
    background-image: url('containers.png');
    background-size: contain;
    background-repeat: no-repeat;
    pointer-events: none;
    z-index: 100;
  }
  header {
    font-size: 14px;
    color: #88a4c2;
    top: 20px;
    left: 60px;
    margin-right: 60px;
  }
  footer {
    font-size: 14px;
    color: #88a4c2;
    bottom: 20px;
    left: 60px;
  }
  section.title, section[id="1"] {
    justify-content: center !important;
    align-content: center !important;
    place-content: center !important;
    padding: 40px 60px !important;
  }
  h1 {
    color: #ffffff;
    font-size: 42px;
  }
  h2 {
    color: #93c5fd;
    font-size: 32px;
    border-bottom: 2px solid #20456e;
    padding-bottom: 8px;
    margin-top: 0 !important;
    margin-bottom: 20px;
    margin-right: 50px;
  }
  h3 {
    color: #7fdbca;
  }
  strong {
    color: #ffffff;
  }
  a {
    color: #7fdbca;
    text-decoration: none;
  }
  code {
    background-color: #081d33;
    color: #ffffff;
    font-weight: 600;
    border: 1px solid #20456e;
    font-size: 85%;
    border-radius: 4px;
    padding: 2px 7px;
  }
  pre {
    background-color: #061525;
    border: 1px solid #20456e;
    border-radius: 8px;
    padding: 18px 24px;
  }
  pre code {
    background-color: transparent !important;
    color: #ffffff !important;
    font-family: 'JetBrains Mono', 'Fira Code', 'SF Mono', Consolas, monospace;
    padding: 0;
  }
  .hljs-comment, .hljs-quote {
    color: #94a3b8 !important;
    font-style: italic;
  }
  .hljs-keyword, .hljs-selector-tag {
    color: #ff7b72 !important;
    font-weight: 600;
  }
  .hljs-string, .hljs-addition {
    color: #7ee787 !important;
  }
  .hljs-number, .hljs-literal {
    color: #ffa657 !important;
  }
  .hljs-attr {
    color: #79c0ff !important;
    font-weight: 600;
  }
  .hljs-built_in {
    color: #ffc259 !important;
  }
  .highlight {
    color: #82aaff;
    font-weight: 600;
  }
  .dim {
    color: #88a4c2;
  }
  blockquote {
    border-left: 4px solid #82aaff;
    background-color: #092038;
    color: #f1f5f9;
    padding: 8px 16px;
  }
  table {
    font-size: 21px;
    color: #f1f5f9;
  }
  th {
    background-color: #081d33;
    color: #ffffff;
    border-bottom: 2px solid #82aaff;
  }
  td {
    border-bottom: 1px solid #20456e;
  }
---

# Incus & Its Legacy
### A 5-Minute Crash Course on System Containers and the Incus Hypervisor

**Speaker:** René Jochum
Linux Containers (`lxc`)

---

## 1. The Concept: System Containers

Docker popularized **application containers** (one process, disposable).
Incus comes from the **LXC** heritage of <span class="highlight">System Containers</span> (a complete Linux OS).

* **It looks, feels, and acts like a Virtual Machine:**
  * Runs a complete Linux distribution: Debian, Ubuntu, Alpine, Arch, Fedora
  * Boots with a real init system (`systemd` or `OpenRC`) as PID 1
  * You can SSH in, create users, install packages with `apt`, and run daemons
* **...But with bare-metal speed:**
  * **Boots in 200ms**, consumes **~25MB RAM** at idle
  * Zero hypervisor overhead: runs directly on host cgroups & namespaces
* **Security by default:**
  * Unprivileged user namespaces (`UID 0` inside $\ne$ root on host) + AppArmor

---

## 2. The Engine: Real Storage & KVM VMs

Incus is not just a container runner—it is a **unified infrastructure manager**:

* **First-Class Storage:**
  * Pluggable drivers for **ZFS, Btrfs, LVM, and Linstor (DRBD)**
  * Instant sub-second copy-on-write snapshots, clones, and block-level quotas
* **The Unified Hypervisor (Containers + VMs):**
  * Need Windows, FreeBSD, or a custom kernel? Incus manages **KVM VMs** side-by-side
  * **One API and CLI for both:**
    ```bash
    incus launch images:debian/trixie my-container   # System Container
    incus launch images:debian/trixie my-vm --vm      # KVM Virtual Machine
    ```

---

## 3. The History: How LXD Became Incus

```
2008: LXC (Linux Containers)
  │   - Foundation of Linux containerization (Docker was originally an LXC wrapper!)
  ▼
2015: LXD ("Lex-Dee")
  │   - The container hypervisor under linuxcontainers.org
  ▼
July 2023: The Canonical Takeover
  │   - Canonical pulls LXD out of linuxcontainers.org, adds CLA, pushes Ubuntu Snaps
  ▼
August 2023: Incus is Born
      - Community fork led by original maintainers under linuxcontainers.org
      - 100% open source (Apache 2.0), no CLA, distro-independent
```

* **Pronunciation:** LXD is <span class="highlight">"Lex-Dee"</span>; Incus is <span class="highlight">"In-kus"</span>

---

## 4. Modern Incus: Native OCI Application Containers

Incus modernized rapidly, solving the final puzzle piece:

* **The Reality:** Developers package applications as **Docker/OCI images**
* **Incus Native OCI:**
  * Incus can now pull directly from `docker.io`, `ghcr.io`, or any OCI registry
  * Runs application containers directly (natively, unprivileged, no nested Docker daemon)
* **The Trifecta in One REST API:**
  1. **OCI Application Containers** (microservices)
  2. **LXC System Containers** (full OS userlands)
  3. **KVM Virtual Machines** (hardware virtualization)

---

## 5. The Bridge: Why `incus-compose`?

We have the ultimate Linux hypervisor. **So what was missing?**

* Developers have millions of `compose.yaml` files
* Nobody wants to write 20 imperative CLI commands to launch a multi-service stack

$\rightarrow$ **`incus-compose` is the drop-in bridge:**
* Point it at your existing `compose.yaml` $\rightarrow$ `incus-compose up -d`
* Runs directly on Incus with official `compose-go` parsing
* Handles health checks, restart policies, and dependency gating via `ic-healthd`
* Instant ZFS/Linstor volume backups and zero-touch ingress

**Now, let's look at the main presentation and see it in action!**
