# Construct cloud operator — the standalone, light agent harness that fires a
# user's automations and answers asks when their desktop is offline. Deployed
# as its own CapRover app `operator`, co-located with Conductor. Stdlib-only Go
# (no go.sum), so the build just needs the source.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/operator .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/operator /operator
ENV PORT=8090
EXPOSE 8090
ENTRYPOINT ["/operator"]
