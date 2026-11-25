(ns limiter.core-test
  (:require [jsonista.core :as json]
            [clojure.test :refer [deftest is testing]]
            [limiter.core :as core]))

(deftest shared-contract
  (doseq [{:keys [name capacity leak_rate steps]}
          (json/read-value (slurp "../contracts/cases.json" :encoding "UTF-8")
                           (json/object-mapper {:decode-key-fn true :bigdecimals true}))]
    (testing name
      (reduce (fn [limiter {:keys [user_id timestamp allowed level updated_at]}]
                (let [[actual next-state] (core/allow-request limiter user_id timestamp)
                      bucket (core/get-bucket-state next-state user_id)]
                  (is (= allowed actual))
                  (is (== level (:level bucket)))
                  (is (== updated_at (:updated_at bucket)))
                  next-state))
              (core/create-rate-limiter capacity leak_rate) steps))))

(deftest snapshots-are-immutable
  (let [empty-state (core/create-rate-limiter 1 0.1)
        [granted? full] (core/allow-request empty-state "alice" 10)
        [second? partial] (core/allow-request full "alice" 11)]
    (is granted?)
    (is (false? second?))
    (is (nil? (core/get-bucket-state empty-state "alice")))
    (is (nil? (core/get-bucket-state full "bob")))
    (is (== 1 (:level (core/get-bucket-state full "alice"))))
    (is (== 0.9M (:level (core/get-bucket-state partial "alice"))))))

(deftest invalid-configurations
  (doseq [[capacity rate] [[0 1] [-1 1] [1.5 1] [1 0] [1 ##NaN] [1 ##Inf]]]
    (is (thrown? IllegalArgumentException (core/create-rate-limiter capacity rate)))))

(deftest microsecond-boundary-at-a-real-epoch
  (let [start 1760000000.123456M
        full (second (core/allow-request (core/create-rate-limiter 1 10) "u" start))
        [early? partial] (core/allow-request full "u" (+ start 0.099999M))
        [on-time? next-state] (core/allow-request partial "u" (+ start 0.100000M))]
    (is (false? early?))
    (is (== 0.000010M (:level (core/get-bucket-state partial "u"))))
    (is on-time?)
    (is (== 1 (:level (core/get-bucket-state next-state "u"))))))
