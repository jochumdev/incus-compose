---
date: 2026-08-08T02:12:01Z
dateCreated: 2026-07-05T04:30:21.277Z
description: Build and serve a Hugo static site on Incus - fast builds, no runtime dependencies, straight from a compose.yaml.
editor: markdown
tags: []
title: Hugo
leafwiki_id: jiajRXfvR
leafwiki_title: Hugo
leafwiki_created_at: "2026-07-05T04:30:21Z"
leafwiki_updated_at: "2026-08-08T02:12:01Z"
leafwiki_creator_id: vOmfrlBDg
leafwiki_last_author_id: vOmfrlBDg
---

# hugo

[Hugo](https://gohugo.io/) is one of the most popular open-source static site
generators: fast builds, no runtime dependencies.

The files for this example are on
[Github](https://github.com/lxc/incus-compose/tree/main/examples/hugo).

## The example

A single `blog` service builds and serves the site via a multi-stage
`Dockerfile` defined inline with `dockerfile_inline`:

1. `alpine` installs Hugo and builds `site/` into `public/`, using the
   `BASE_URL` build arg for `hugo build -b`.
2. `nginxinc/nginx-unprivileged:alpine` serves the built `public/` directory.

`compose.incus.yaml` overrides `BASE_URL` with the container's static IP so Hugo
generates correct absolute links, and adds the static IP, a healthcheck, and a
CPU limit.

## Usage

```bash
incus-compose up
```

Open http://10.134.32.17:8080/
