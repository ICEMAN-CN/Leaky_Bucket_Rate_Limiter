(ns limiter.core-test
  (:require [clojure.data.json :as json]
            [clojure.test :refer [deftest is testing]]
            [limiter.core :as core]))

(deftest shared-contract
  (doseq [{:keys [name capacity leak_rate steps]}
          (json/read-str (slurp "../contracts/cases.json") :key-fn keyword :bigdec true)]
    (testing name
      (reduce (fn [limiter {:keys [user_id timestamp allowed level as_of]}]
                (let [[actual next-state] (core/allow-request limiter user_id timestamp)
                      bucket (core/get-bucket-state next-state user_id)]
                  (is (= allowed actual))
                  (is (== level (:level bucket)))
                  (is (== as_of (:as_of bucket)))
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
