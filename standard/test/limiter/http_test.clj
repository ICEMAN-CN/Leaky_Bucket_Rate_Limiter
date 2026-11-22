(ns limiter.http-test
  (:require [clojure.data.json :as json]
            [clojure.test :refer [deftest is]]
            [limiter.core :as core]
            [limiter.http :as http]
            [limiter.server :as server])
  (:import [java.io ByteArrayInputStream]
           [java.net URI]
           [java.net.http HttpClient HttpRequest HttpResponse$BodyHandlers]
           [java.nio.charset StandardCharsets]))

(defn- post [app payload]
  (app {:request-method :post :uri "/v1/check"
        :body (ByteArrayInputStream. (.getBytes ^String payload StandardCharsets/UTF_8))}))

(defn- query [app uri]
  (app {:request-method :get :uri uri}))

(defn- data [res]
  (json/read-str (:body res) :key-fn keyword))

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
      (is (== 7 (get-in (data first-response) [:bucket :as_of]))))
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
                          #(throw (ex-info "测试时钟故障" {})))]
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
        (is (= "test" (get (json/read-str (.body res)) "instance"))))
      (finally (.stop server)))))
