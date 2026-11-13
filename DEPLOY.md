# Deploying Vantage in front of www.leo-zhang.com

Vantage runs on Fly.io and sits between visitors and the site's Vercel
deployment. Visitors keep typing `leo-zhang.com`; only a DNS record changes.

```
before:  visitor ──▶ Vercel (www.leo-zhang.com)
after:   visitor ──▶ Fly.io: Vantage (Toronto) ──▶ Vercel (<project>.vercel.app)
```

`leo-zhang.com` (the apex) already redirects to `www.leo-zhang.com` on Vercel.
Leave the apex alone; only `www` moves. Its redirect keeps sending visitors to
`www`, which now reaches Vantage.

## What it costs and what can go wrong

- **Cost:** one always-on `shared-cpu-1x` machine (256 MB) plus a 1 GB volume,
  a few dollars a month. Check current rates at fly.io/pricing. Fly asks for a
  card at signup.
- **Latency:** each visit adds a hop (visitor → Toronto → Vercel's edge),
  typically 10–30 ms. Vercel's cache still works behind it.
- **Availability:** if Vantage is down, the site is down. Fly restarts it on
  failed health checks, and rollback is deleting one DNS record (see the end).
- **Host header:** Vercel sees the request for `<project>.vercel.app`, with the
  original host in `X-Forwarded-Host`. A static or ordinary Next.js site doesn't
  care. If the site builds absolute URLs from the request host (for example, OG
  image URLs), check them after the switch.

## 1. Find the Vercel origin

In the Vercel dashboard: **Project → Settings → Domains**. Note the
`<project>.vercel.app` domain. Then confirm it serves the same site and is
public:

```bash
curl -sI https://<project>.vercel.app | head -1     # expect HTTP/1.1 200 OK
```

If it returns 401 or a Vercel login page, it is behind Deployment Protection.
Allow public access to the production `.vercel.app` domain (**Settings →
Deployment Protection**) before going further.

## 2. Configure and deploy to Fly

```bash
# install flyctl: https://fly.io/docs/flyctl/install/
fly auth login
```

Edit `fly.toml`: set `app` to a unique name and replace
`REPLACE-WITH-YOUR-PROJECT.vercel.app` with the origin from step 1. Then:

```bash
fly launch --no-deploy --copy-config --name <app-name>
fly volumes create vantage_data --size 1 --region yyz
fly secrets set VANTAGE_REPORT_PASSWORD="$(openssl rand -base64 24)"   # save this password
fly deploy
```

Test it before touching DNS. The site should load through Fly:

```bash
curl -sI https://<app-name>.fly.dev | head -1                 # 200 from your site
curl -s https://<app-name>.fly.dev/_vantage/healthz           # ok
```

Then open `https://<app-name>.fly.dev/_vantage/report` in a browser. It should
ask for a password (any username works).

## 3. Get the certificate before moving traffic

```bash
fly certs add www.leo-zhang.com
fly certs setup www.leo-zhang.com
```

The second command prints a DNS validation record, a CNAME for
`_acme-challenge.www.leo-zhang.com`. Add it in Vercel (**Domains →
leo-zhang.com → DNS Records**), then wait until `fly certs check
www.leo-zhang.com` reports the certificate as issued. Fly now has a valid
certificate before any traffic arrives, so the switch has no HTTPS errors.

## 4. Switch www to Fly

The domain's DNS is hosted by Vercel (nameservers `ns1/ns2.vercel-dns.com`).
In **Domains → leo-zhang.com → DNS Records**, add:

| Name | Type | Value | TTL |
|---|---|---|---|
| `www` | `CNAME` | `<app-name>.fly.dev` | 60 |

If Vercel refuses because `www` is assigned to the project, remove
`www.leo-zhang.com` from **Project → Settings → Domains**. Then make sure the
apex `leo-zhang.com` still redirects to `www.leo-zhang.com`, or point the apex
at the project directly.

Verify:

```bash
curl -sI https://www.leo-zhang.com | grep -i fly-request-id   # present = through Vantage
```

## 5. Read the report

- **Browser:** `https://www.leo-zhang.com/_vantage/report`, with the password from step 2.
- **Live dashboard:** `fly proxy 9090:9090`, then open `http://localhost:9090`.
- **Raw file:** `fly ssh console -C "cat /data/report.md"`.

## Rollback

Delete the `www` CNAME record in Vercel DNS (and re-add `www.leo-zhang.com` to
the project if you removed it). With a 60 s TTL, traffic goes straight back to
Vercel within minutes. The Fly app can stay or be destroyed with
`fly apps destroy <app-name>`.
