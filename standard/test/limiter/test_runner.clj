(ns limiter.test-runner
  (:require [clojure.test :as test]
            [limiter.core-test]
            [limiter.http-test]))

(defn -main [& _]
  (let [{:keys [fail error]} (test/run-tests 'limiter.core-test 'limiter.http-test)]
    (shutdown-agents)
    (when (pos? (+ fail error)) (System/exit 1))))
