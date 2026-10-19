FROM node:22-alpine AS web
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend ./
RUN npm run build

FROM golang:1.23-alpine AS api
WORKDIR /app/backend
COPY backend/go.mod ./
COPY backend/*.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /dashboard .

FROM alpine:3.20
RUN adduser -D -H app
WORKDIR /app
COPY --from=api /dashboard /usr/local/bin/dashboard
COPY --from=web /app/frontend/dist /app/frontend/dist
USER app
ENV PORT=8080 STATIC_DIR=/app/frontend/dist
EXPOSE 8080
ENTRYPOINT ["dashboard"]
