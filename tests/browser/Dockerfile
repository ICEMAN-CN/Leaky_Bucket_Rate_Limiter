FROM mcr.microsoft.com/playwright:v1.61.0-noble
WORKDIR /suite
COPY tests/browser/package*.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund && npm cache clean --force
COPY tests/browser/ ./
ENV EXTERNAL_STANDARD_URL=http://clojure:8080
CMD ["npm", "test"]
