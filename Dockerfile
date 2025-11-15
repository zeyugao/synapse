FROM docker.io/library/golang:1.25 AS build

WORKDIR /go/src/app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG SEMVER=0.0.0
RUN make -j VERSION=${VERSION} SEMVER=${SEMVER}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /go/src/app/bin/server /go/src/app/bin/client /usr/bin/
