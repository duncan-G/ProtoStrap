# The edge (docs/protostrap.md#edge) runs cloudflared and Envoy in one container, so the
# pair is always placed, started and stopped together. `protostrap edge` runs both, answers
# the agent's readiness check, and serves Envoy's request counts.
#
# The deploy workflow builds this image once per ProtoStrap version, with that version's
# protostrap binary copied in beside this file, and pushes it to protostrap/edge:<version>.
# cloudflared's version is pinned here, so the tunnel never updates itself.
FROM cloudflare/cloudflared:2026.10.7 AS cloudflared

FROM envoyproxy/envoy:v1.40.2
COPY --from=cloudflared /usr/local/bin/cloudflared /usr/local/bin/cloudflared
COPY protostrap /usr/local/bin/protostrap
USER envoy
ENTRYPOINT ["/usr/local/bin/protostrap", "edge"]
