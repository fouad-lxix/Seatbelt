# Seatbelt

Plug it in, and stop worrying about your AI plumbing. For good.

Seatbelt sits between your app and the AI companies (Anthropic's Claude and
OpenAI's GPT models) and takes every one of the annoying, expensive problems
that come with calling them at scale, and handles them for you, permanently.
You will never blow through a budget by accident. You will never get taken
down by a burst of traffic. A flaky moment on the provider's end will never
take your app down with it.

Set it up once. Point your app at it. Forget it's there.

Built in Go, it's small, fast, and stays out of your way, one lightweight
process with nothing extra bolted on.

```sh
docker run --rm -p 8080:8080 -e ANTHROPIC_API_KEY=sk-ant-... seatbelt
```

That's the whole install.

---

## The problem this solves

If you or your team use Claude or GPT for anything beyond the occasional
question, you will run into these, it's not a matter of if:

- **You get billed more than you expected**, because nothing was stopping
  the spend once it started.
- **Requests get rejected** because too many were sent at once, and now part
  of your app is broken until someone notices.
- **A request fails for a silly, temporary reason** (a brief hiccup on the
  provider's end) and instead of automatically trying again, the whole job
  just fails.

Every one of these is fixable with enough extra code, monitoring, and
on-call attention. Seatbelt fixes all three permanently, in one place,
before they ever reach your app, so nobody on your team has to build or
babysit that code.

## What it actually does for you

**It stops you from overspending. Completely.** You tell it a dollar limit,
once. From that moment on, Seatbelt stops making new calls the instant the
limit is hit and tells you clearly. Not a warning you might miss. A hard
stop.

**It never lets your requests overwhelm the provider.** You decide how many
requests can be happening at once. Everything beyond that waits its turn
automatically, instead of all crashing into the provider at the same time
and getting rejected. Your app just works, even under a burst.

**It automatically fixes the failures worth fixing.** If a request fails for
a reason that's likely temporary, Seatbelt tries again for you, waiting a
little longer each time, no code, no retry logic, no on-call page. If a
request fails because something is genuinely wrong with it, it does not
waste time retrying, it tells you immediately so you can fix it.

**It never leaves you hanging.** If it's too busy to take more work right
now, you get an immediate, clear answer instead of a request that quietly
sits there. You always know exactly where you stand.

**It shows you everything, live, with zero setup.** A simple page you open
in your browser shows current spend, what's running, and how everything is
going, updated automatically. No dashboard to configure, no third-party
tool to wire up.

Point it once. Never touch it again.

## Setting it up

The easiest way to run Seatbelt is with Docker, a tool that runs software in
a self-contained package so you don't have to install anything else on your
machine. If you don't already have Docker, install it from
[docker.com](https://www.docker.com/) first.

Once Docker is installed, open a terminal in this project folder and run:

```sh
docker build -t seatbelt .
docker run --rm -p 8080:8080 \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e MAX_SPEND_USD=5.00 \
  --name seatbelt seatbelt
```

Replace `sk-ant-...` with your real Anthropic API key (the code you get from
your Anthropic account that lets you use Claude), and `5.00` with the dollar
limit you want to set. That's it. Seatbelt is running, and everything below
is now handled for you automatically.

To stop it, press `Ctrl-C` in that same terminal, or run
`docker stop -t 30 seatbelt` from another one. Either way, it finishes
answering anything already in progress before it shuts down, so nothing gets
cut off mid-request.

## Pointing your app at it

This is the entire integration. Once Seatbelt is running, the only change
your app needs is where it sends its requests. Instead of sending them
straight to Anthropic or OpenAI, it sends them to Seatbelt, and Seatbelt
forwards them and hands back the exact same answer the provider would have
given. Nothing else about your app changes, ever.

```diff
- https://api.anthropic.com/v1/messages
+ http://localhost:8080/v1/anthropic/messages

- https://api.openai.com/v1/chat/completions
+ http://localhost:8080/v1/openai/chat/completions
```

The API key you'd normally send is not needed either, Seatbelt already has
it from when you set it up. Change the URL, and every protection above is
active immediately.

## Watching it work

While Seatbelt is running, open this in a browser:

```
http://localhost:8080/stats/ui
```

It shows, updated automatically: how much you've spent and how much budget
is left, how many requests are running right now, how many have succeeded or
failed, and how many are waiting in line. Nothing to install, it's just a
web page, always current.

## Setup options

You can adjust how Seatbelt behaves by setting these before you run it (see
[`.env.example`](.env.example) for the full list with examples).

| What you set | Default | What it controls |
| --- | --- | --- |
| `ANTHROPIC_API_KEY` | none | Your Claude key. Set this to enable Claude. |
| `OPENAI_API_KEY` | none | Your OpenAI key. Set this to enable GPT. |
| `MAX_SPEND_USD` | $5.00 | The hard spending limit. |
| `WORKERS_PER_PROVIDER` | 4 | How many requests can run at the same time. |
| `RATE_LIMIT_PER_PROVIDER` | 60 | How many requests per minute are allowed. |
| `MAX_QUEUE_DEPTH` | 64 | How many requests can wait in line before new ones are turned away. |
| `PORT` | 8080 | Which port Seatbelt listens on. |

You need at least one of the two API keys. Everything else already has a
sensible default, tuned to work well out of the box, you only need to
change it if you want to.

## Keeping prices accurate

AI companies change their prices from time to time. Seatbelt checks for
updated pricing on its own, every day, and double-checks anything it
downloads before trusting it, automatically, with no action from you. If it
can't reach the update or something looks wrong with it, it keeps using the
last prices it trusted, so your spending limit is never silently wrong.

If you'd rather it never checked for updates automatically, set
`PRICING_REFRESH=false` when you run it.

## Coming soon

Seatbelt is under active development. Here's what's landing next.

**Streaming.** Answers that arrive word by word as they're generated,
working straight through Seatbelt with every protection it already gives
you, budget cap, retries, and all.

**Separate budgets per project or key.** One Seatbelt, shared by a whole
team, with each project or app getting its own spending limit instead of
one number for everyone.

**Support for more providers.** Google's Gemini and others, behind the
exact same plug in and forget setup you already have.
