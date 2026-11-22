(ns limiter.core)

(defn- finite-number? [n]
  (and (number? n) (Double/isFinite (double n))))

(defn create-rate-limiter
  "创建一个空的全局限流器。每个用户独享整桶容量。"
  [capacity leak-rate]
  (when-not (and (pos-int? capacity)
                 (finite-number? leak-rate) (pos? leak-rate))
    (throw (IllegalArgumentException. "容量必须是正整数，漏速必须是有限正数")))
  {:capacity capacity :leak_rate (bigdec leak-rate) :buckets {}})

(defn allow-request
  "按给定的 Unix 秒结算漏量，返回 [是否放行 新状态]。"
  [{:keys [capacity leak_rate buckets] :as limiter} user-id timestamp]
  (when-not (and (string? user-id) (not (empty? user-id)) (finite-number? timestamp))
    (throw (IllegalArgumentException. "需要非空字符串用户 ID 和有限时间戳")))
  (let [now (bigdec timestamp)
        {:keys [level as_of]} (get buckets user-id {:level 0M :as_of now})
        settled-at (max as_of now)
        drained (max 0M (- level (* leak_rate (- settled-at as_of))))
        allowed? (<= (inc drained) capacity)
        bucket {:level (if allowed? (inc drained) drained) :as_of settled-at}]
    [allowed? (assoc-in limiter [:buckets user-id] bucket)]))

(defn get-bucket-state
  "读取最近一次判定的快照；未知用户返回 nil。"
  [limiter user-id]
  (when-let [bucket (get-in limiter [:buckets user-id])]
    (merge bucket (select-keys limiter [:capacity :leak_rate]))))
