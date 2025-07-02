# ---------- build ----------
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy && go mod download
RUN go build -o /out/auth-extproc .

# ---------- runtime ----------
FROM alpine:3.19
WORKDIR /bin
COPY --from=build /out/auth-extproc /bin/
ENTRYPOINT ["/bin/auth-extproc"]
