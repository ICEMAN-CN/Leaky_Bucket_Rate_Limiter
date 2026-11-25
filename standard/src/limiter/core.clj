(ns limiter.core)

(defn- finite-number? [n]
  (and (number? n) (Double/isFinite (double n))))

(defn create-rate-limiter
  "Creates an empty global limiter. Each user gets a bucket with the full capacity."
  [capacity leak-rate]
  (when-not (and (pos-int? capacity)
                 (finite-number? leak-rate) (pos? leak-rate))
    (throw (IllegalArgumentException. "Capacity must be a positive integer and leak rate must be positive and finite")))
  {:capacity capacity :leak_rate (bigdec leak-rate) :buckets {}})

(defn allow-request
  "Settles leakage at the given Unix time in seconds and returns [allowed? next-state]."
  [{:keys [capacity leak_rate buckets] :as limiter} user-id timestamp]
  (when-not (and (string? user-id) (not (empty? user-id)) (finite-number? timestamp))
    (throw (IllegalArgumentException. "A nonempty user ID string and a finite timestamp are required")))
  (let [now (bigdec timestamp)
        {:keys [level updated_at]} (get buckets user-id {:level 0M :updated_at now})
        settle_time (max updated_at now)
        remaining (max 0M (- level (* leak_rate (- settle_time updated_at))))
        allowed? (<= (inc remaining) capacity)
        bucket {:level (if allowed? (inc remaining) remaining) :updated_at settle_time}]
    [allowed? (assoc-in limiter [:buckets user-id] bucket)]))

(defn get-bucket-state
  "Returns the last settled snapshot, or nil for an unknown user."
  [limiter user-id]
  (when-let [bucket (get-in limiter [:buckets user-id])]
    (merge bucket (select-keys limiter [:capacity :leak_rate]))))
