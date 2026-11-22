(ns limiter.server
  (:require [limiter.core :as core]
            [limiter.http :as http]
            [ring.adapter.jetty :as jetty])
  (:import [org.eclipse.jetty.http UriCompliance]
           [org.eclipse.jetty.server HttpConnectionFactory Server]))

(defn- configure-paths! [^Server server]
  ;; 路由匹配原始路径后只解码一次，用户 ID 可包含编码后的斜杠和百分号。
  (doseq [connector (.getConnectors server)]
    (let [factory (.getConnectionFactory connector HttpConnectionFactory)]
      (.setUriCompliance (.getHttpConfiguration factory)
                         (UriCompliance/from "DEFAULT,AMBIGUOUS_PATH_SEPARATOR,AMBIGUOUS_PATH_ENCODING")))))

(defn start! [handler port]
  (jetty/run-jetty handler {:port port :join? false :send-server-version? false
                            :configurator configure-paths!}))

(defn- env [key default] (or (System/getenv key) default))

(defn -main [& _]
  (let [limiter (core/create-rate-limiter (Long/parseLong (env "CAPACITY" "5"))
                                         (bigdec (env "LEAK_RATE" "1.0")))
        app (http/handler limiter (env "INSTANCE_ID" "clojure")
                          #(/ (System/currentTimeMillis) 1000M))
        server (start! app (Integer/parseInt (env "PORT" "8080")))]
    (.setStopAtShutdown server true)
    (.join server)))
