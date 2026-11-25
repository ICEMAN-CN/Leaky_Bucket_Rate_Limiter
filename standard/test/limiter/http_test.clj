(ns limiter.http-test
  (:require [jsonista.core :as json]
            [clojure.test :refer [deftest is]]
            [limiter.core :as core]
            [limiter.http :as http]
            [limiter.server :as server])
  (:import [java.io ByteArrayInputStream]
           [java.net URI]
           [java.net.http HttpClient HttpRequest HttpRequest$BodyPublishers HttpResponse$BodyHandlers]
           [java.nio.charset StandardCharsets]))

(defn- post
  ([app payload] (post app "/v1/check" payload))
  ([app uri payload]
   (let [bytes (if (string? payload) (.getBytes ^String payload StandardCharsets/UTF_8) payload)]
     (app {:request-method :post :uri uri :body (ByteArrayInputStream. bytes)}))))

(defn- query [app uri]
  (app {:request-method :get :uri uri}))

(defn- data [res]
  (json/read-value (:body res) json/keyword-keys-object-mapper))

(deftest http-contract
  (let [app (http/handler (core/create-rate-limiter 1 1) "test" (constantly 7))
        check #(post app "{\"user_id\":\"a/b+%\"}")
        uri "/v1/buckets/a%2Fb+%25"]
    (is (= 404 (:status (query app uri))))
    (is (= 200 (:status (query app "/livez"))))
    (is (= 200 (:status (query app "/readyz"))))
    (let [first-response (check)]
      (is (= 200 (:status first-response)))
      (is (true? (:allowed (data first-response))))
      (is (== 7 (get-in (data first-response) [:bucket :updated_at]))))
    (let [second-response (check)]
      (is (= 429 (:status second-response)))
      (is (false? (:allowed (data second-response))))
      (is (= (:bucket (data second-response)) (:bucket (data (query app uri))))))
    (doseq [bad ["{}" "null" "{\"user_id\":\"\"}" "{\"user_id\":4}" "{bad" "{}{}" "{\"user_id\":\"x\",\"timestamp\":900}"]]
      (is (= 400 (:status (post app bad)))))
    (let [{:keys [allowed limited errors latency_bins started_at]}
          (data (query app "/internal/metrics"))]
      (is (= [1 1 0] [allowed limited errors]))
      (is (seq started_at))
      (is (= 8 (count latency_bins)))
      (is (= 2 (reduce + latency_bins))))))

(deftest one-admission-for-the-last-slot
  (let [app (http/handler (core/create-rate-limiter 5 1) "test" (constantly 0))
        gate (promise)
        calls (doall (repeatedly 20 #(future @gate (:status (post app "{\"user_id\":\"one\"}")))))]
    (deliver gate true)
    (is (= {200 5 429 15} (frequencies (map deref calls))))))

(deftest business-error-metrics
  (let [app (http/handler (core/create-rate-limiter 1 1) "test"
                          #(throw (ex-info "Test clock failure" {})))]
    (is (= 503 (:status (post app "{\"user_id\":\"alice\"}"))))
    (let [metrics (data (query app "/internal/metrics"))]
      (is (= 1 (:errors metrics)))
      (is (= 1 (reduce + (:latency_bins metrics)))))))

(deftest encoded-id-through-jetty
  (let [app (http/handler (core/create-rate-limiter 5 1) "test" (constantly 0))
        _ (post app "{\"user_id\":\"a/b+%\"}")
        server (server/start! app 0)]
    (try
      (let [port (.getLocalPort (first (.getConnectors server)))
            uri (URI/create (str "http://127.0.0.1:" port "/v1/buckets/a%2Fb%2B%25"))
            req (.build (.GET (HttpRequest/newBuilder uri)))
            res (.send (HttpClient/newHttpClient) req (HttpResponse$BodyHandlers/ofString))]
        (is (= 200 (.statusCode res)) (.body res))
        (is (= "test" (get (json/read-value (.body res)) "instance"))))
      (finally (.stop server)))))

(deftest strict-json-for-both-post-routes
  (let [app (http/handler (core/create-rate-limiter 1 1) "test" (constantly 0))
        invalid-utf8 (byte-array (concat (.getBytes "{\"user_id\":\"" StandardCharsets/UTF_8)
                                         [-1] (.getBytes "\"}" StandardCharsets/UTF_8)))]
    (doseq [uri ["/v1/check" "/v1/bucket-state"]
            payload ["{\"user_id\":\"a\nb\"}" "{\"user_id\":\"a\"}{}"
                     "{\"user_id\":\"\"}" "{\"USER_ID\":\"x\"}" invalid-utf8]]
      (is (= 400 (:status (post app uri payload)))))))

(deftest body-snapshot-through-jetty
  (let [now (atom 10)
        app (http/handler (core/create-rate-limiter 5 1) "test" #(deref now))
        server (server/start! app 0)
        port (.getLocalPort (first (.getConnectors server)))
        client (HttpClient/newHttpClient)
        send-post (fn [path payload]
                    (let [url (URI/create (str "http://127.0.0.1:" port path))
                          req (-> (HttpRequest/newBuilder url)
                                  (.header "Content-Type" "application/json")
                                  (.POST (HttpRequest$BodyPublishers/ofString payload))
                                  (.build))]
                      (.send client req (HttpResponse$BodyHandlers/ofString))))
        max-id (apply str (repeat (- 65536 (count "{\"user_id\":\"\"}")) "x"))
        ids ["." ".." "\u6e2f/ +%?#" "a\nb" max-id]]
    (try
      (doseq [id ids]
        (let [payload (json/write-value-as-string {:user_id id})
              check (send-post "/v1/check" payload)
              snapshot #(send-post "/v1/bucket-state" payload)]
          (is (= 200 (.statusCode check)))
          (swap! now inc)
          (dotimes [_ 2]
            (let [read (snapshot)]
              (is (= 200 (.statusCode read)))
              (is (= (get (json/read-value (.body check)) "bucket")
                     (get (json/read-value (.body read)) "bucket")))))))
      (let [unknown (send-post "/v1/bucket-state" "{\"user_id\":\"unknown\"}")]
        (is (= 404 (.statusCode unknown)))
        (is (= "bucket_not_found" (get (json/read-value (.body unknown)) "error"))))
      (doseq [path ["/v1/check" "/v1/bucket-state"]]
        (is (= 400 (.statusCode (send-post path (json/write-value-as-string {:user_id (str max-id "x")}))))))
      (let [{:keys [allowed limited errors latency_bins]} (data (query app "/internal/metrics"))]
        (is (= [(count ids) 0 0] [allowed limited errors]))
        (is (= allowed (reduce + latency_bins))))
      (finally (.stop server)))))
