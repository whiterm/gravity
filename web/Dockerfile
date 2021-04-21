FROM node:12.18.3-buster

RUN apt-get update && apt-get --no-install-recommends -y install git

RUN mkdir -p /app
COPY package*.json /app/

# copy the rest of the app files
WORKDIR /app

RUN set -ex && \
    npm set audit false && \
    npm install --silent

COPY . /app

# and build
RUN set -ex && \
    npm run build

# tar up the resulting build
RUN tar -C /app/dist -czf /web-assets.tar.gz .
