(ns limiter.http
  (:require [jsonista.core :as json]
            [clojure.string :as str]
            [limiter.core :as core])
  (:import [com.fasterxml.jackson.databind DeserializationFeature]
           [java.io IOException InputStream]
           [java.net URLDecoder]
           [java.nio ByteBuffer]
           [java.nio.charset StandardCharsets]
           [java.util UUID]))

(defn- response [status data]
  {:status status
   :headers {"Content-Type" "application/json; charset=utf-8"
             "Cache-Control" "no-store"}
   :body (json/write-value-as-string data)})

(defn- error-response [instance status error]
  (response status {:instance instance :error error}))

(def ^:private max-body-bytes (* 64 1024))

(def ^:private request-mapper
  (doto (json/object-mapper)
    (.enable DeserializationFeature/FAIL_ON_TRAILING_TOKENS)))

(defn- read-user-id [^InputStream body]
  (when body
    (try
      (let [bytes (.readNBytes body (inc max-body-bytes))]
        (when (<= (alength bytes) max-body-bytes)
          (let [decoder (.newDecoder StandardCharsets/UTF_8)
                text (str (.decode decoder (ByteBuffer/wrap bytes)))
                payload (json/read-value text request-mapper)
                id (get payload "user_id")]
            (when (and (map? payload) (= 1 (count payload)) (string? id) (seq id)) id))))
      (catch IOException _ nil))))

(defn- decode-id [raw-id]
  ;; URLDecoder 按表单规则解释加号。路径中的字面加号属于用户 ID。
  (URLDecoder/decode (str/replace raw-id "+" "%2B") StandardCharsets/UTF_8))

(defn- snapshot [state instance user-id]
  (if-let [bucket (core/get-bucket-state @state user-id)]
    (response 200 {:instance instance :bucket bucket})
    (error-response instance 404 "bucket_not_found")))

(defn- admit! [state user-id now]
  ;; 将一次纯函数判定与提交放在同一临界区，避免 Jetty 的线程争用最后一个名额。
  (locking state
    (let [[allowed? next-state] (core/allow-request @state user-id (now))]
      (reset! state next-state)
      [allowed? (core/get-bucket-state next-state user-id)])))

(def latency-bounds-ms [1 5 10 25 50 100 500])

(defn- record-check! [metrics outcome started-ns]
  (let [elapsed-ms (/ (- (System/nanoTime) started-ns) 1e6)
        bin (count (take-while #(< % elapsed-ms) latency-bounds-ms))]
    (swap! metrics #(-> % (update outcome inc) (update-in [:latency_bins bin] inc)))))

(defn- check [state metrics instance now body]
  (let [started-ns (System/nanoTime)]
    (if-let [user-id (read-user-id body)]
      (try
        (let [[allowed? bucket] (admit! state user-id now)]
          (record-check! metrics (if allowed? :allowed :limited) started-ns)
          (response (if allowed? 200 429)
                    {:allowed allowed? :instance instance :bucket bucket}))
        (catch Exception e
          (record-check! metrics :errors started-ns)
          (binding [*out* *err*] (println "限流判定失败" (.getMessage e)))
          (error-response instance 503 "limiter_unavailable")))
      (error-response instance 400 "invalid_request"))))

(defn handler
  "每个服务实例创建一次。生产时钟与测试时钟使用相同接口，均返回 Unix 秒。"
  [limiter instance now]
  (let [state (atom limiter)
        metrics (atom {:instance instance :started_at (str (UUID/randomUUID))
                       :allowed 0 :limited 0 :errors 0 :latency_bins (vec (repeat 8 0))})]
    (fn [{:keys [request-method uri body]}]
      (cond
        (and (= :post request-method) (= "/v1/check" uri))
        (check state metrics instance now body)

        (and (= :post request-method) (= "/v1/bucket-state" uri))
        (if-let [user-id (read-user-id body)]
          (snapshot state instance user-id)
          (error-response instance 400 "invalid_request"))

        (and (= :get request-method) (str/starts-with? uri "/v1/buckets/"))
        (if-let [[_ raw-id] (re-matches #"/v1/buckets/([^/]+)" uri)]
          (try
            (snapshot state instance (decode-id raw-id))
            (catch IllegalArgumentException _
              (error-response instance 400 "invalid_request")))
          (error-response instance 404 "route_not_found"))

        (and (= :get request-method) (= "/internal/metrics" uri))
        (response 200 @metrics)

        (and (= :get request-method) (#{"/livez" "/readyz"} uri))
        (response 200 {:instance instance :status "ok"})

        :else (error-response instance 404 "route_not_found")))))
