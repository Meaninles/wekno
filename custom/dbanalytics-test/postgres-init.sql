CREATE SCHEMA IF NOT EXISTS ops;
CREATE SCHEMA IF NOT EXISTS stg;
CREATE SCHEMA IF NOT EXISTS shadow;

CREATE TABLE ops.cust (
  cust_id BIGINT PRIMARY KEY,
  nm TEXT,
  seg TEXT,
  reg_cd TEXT,
  joined_dt DATE,
  vip_flg BOOLEAN,
  raw_note TEXT
);
COMMENT ON TABLE ops.cust IS 'customer master, mixed quality upstream CRM records';
COMMENT ON COLUMN ops.cust.cust_id IS 'customer id';
COMMENT ON COLUMN ops.cust.seg IS 'customer segment, values are not fully normalized';

CREATE TABLE ops.ord_hdr (
  oid BIGINT PRIMARY KEY,
  cust_id BIGINT,
  odt TIMESTAMP,
  stat_cd TEXT,
  gross_amt NUMERIC(12,2),
  disc_amt NUMERIC(12,2),
  pay_amt NUMERIC(12,2),
  ch TEXT,
  src_cd TEXT
);
COMMENT ON TABLE ops.ord_hdr IS 'order header facts';
COMMENT ON COLUMN ops.ord_hdr.pay_amt IS 'actual paid amount after discount';

CREATE TABLE ops.ord_ln (
  line_id BIGINT PRIMARY KEY,
  oid BIGINT,
  sku TEXT,
  qty INTEGER,
  unit_px NUMERIC(12,2),
  promo_cd TEXT,
  wh TEXT
);
COMMENT ON COLUMN ops.ord_ln.qty IS 'quantity, may be negative for manual corrections';

CREATE TABLE ops.sku_dim (
  sku TEXT PRIMARY KEY,
  pnm TEXT,
  cat1 TEXT,
  cat2 TEXT,
  brand TEXT,
  cost_amt NUMERIC(12,2)
);
COMMENT ON TABLE ops.sku_dim IS 'SKU/product dimension';

CREATE TABLE ops.pay_evt (
  pay_id BIGINT PRIMARY KEY,
  oid BIGINT,
  pay_ts TIMESTAMP,
  method TEXT,
  amt NUMERIC(12,2),
  ok_flg BOOLEAN
);

CREATE TABLE ops.refund_x (
  rid BIGINT PRIMARY KEY,
  oid BIGINT,
  r_dt TIMESTAMP,
  amt NUMERIC(12,2),
  why TEXT,
  state TEXT
);
COMMENT ON TABLE ops.refund_x IS 'refund events, state is inconsistent across channels';

CREATE TABLE ops.traffic_daily (
  dt DATE,
  ch TEXT,
  camp_id TEXT,
  uv INTEGER,
  clk INTEGER,
  spend NUMERIC(12,2)
);

CREATE TABLE stg.x_mkt_cmp (
  camp_id TEXT PRIMARY KEY,
  nm TEXT,
  chn TEXT,
  owner TEXT,
  budget_amt NUMERIC(12,2),
  start_dt DATE,
  end_dt DATE
);
COMMENT ON COLUMN stg.x_mkt_cmp.budget_amt IS 'planned media budget';

CREATE TABLE shadow.tmp_2024_x (
  k TEXT,
  v TEXT,
  dt TEXT
);

CREATE TABLE shadow.job_audit_evt (
  job_nm TEXT,
  ts TIMESTAMP,
  lvl TEXT,
  msg TEXT
);

INSERT INTO ops.cust VALUES
(1001,'Ava Chen','vip','E',DATE '2025-01-06',true,'prefers email'),
(1002,'Noah Li','new','N',DATE '2025-02-18',false,NULL),
(1003,'Mia Zhang','wholesale','S',DATE '2024-11-02',false,'contract price'),
(1004,'Kai Wang','VIP','E',DATE '2025-03-15',true,'duplicate phone?'),
(1005,'Lina Xu','normal','W',DATE '2025-01-28',false,''),
(1006,'Ben Q','trial','N',DATE '2025-04-01',false,'imported from event');

INSERT INTO ops.sku_dim VALUES
('sku-001','Aurora Coffee Beans','grocery','coffee','Northstar',38.20),
('sku-002','Orbit Tea Pack','grocery','tea','Orbit',18.10),
('sku-003','Pulse Blender','appliance','kitchen','Pulse',210.00),
('sku-004','Zen Bottle 500ml','outdoor','drinkware','Zen',42.50),
('sku-005','Nova Headset','electronics','audio','Nova',159.90),
('bad-sku','unknown item',NULL,NULL,NULL,NULL);

INSERT INTO ops.ord_hdr VALUES
(90001,1001,'2026-05-01 09:31:00','paid',320.00,20.00,300.00,'app','ad'),
(90002,1002,'2026-05-01 14:22:00','P',178.20,0.00,178.20,'web','seo'),
(90003,1003,'2026-05-02 10:05:00','paid',1280.00,120.00,1160.00,'sales','offline'),
(90004,1001,'2026-05-03 16:44:00','refunding',459.70,30.00,429.70,'app','push'),
(90005,1004,'2026-05-04 11:00:00','cancel',88.00,0.00,0.00,'web','ad'),
(90006,1005,'2026-05-04 21:08:00','paid',249.90,10.00,239.90,'mini','kol'),
(90007,1006,'2026-05-05 08:17:00','paid',620.00,0.00,620.00,'app','ad'),
(90008,1002,'2026-05-06 12:47:00','paid',58.00,0.00,58.00,'web','seo'),
(90009,1004,'2026-05-07 19:02:00','PAID',999.00,90.00,909.00,'app','push'),
(90010,1003,'2026-05-08 09:15:00','bad_status',42.00,0.00,42.00,NULL,'unknown');

INSERT INTO ops.ord_ln VALUES
(1,90001,'sku-001',2,120.00,'MAY20','east-01'),
(2,90001,'sku-004',1,80.00,NULL,'east-01'),
(3,90002,'sku-002',3,59.40,NULL,'north-02'),
(4,90003,'sku-003',4,320.00,'B2B','south-01'),
(5,90004,'sku-005',1,459.70,'PUSH30','east-01'),
(6,90005,'sku-004',1,88.00,NULL,'east-02'),
(7,90006,'sku-005',1,249.90,'KOL10','west-01'),
(8,90007,'sku-003',2,310.00,NULL,'north-02'),
(9,90008,'sku-002',1,58.00,NULL,'north-02'),
(10,90009,'sku-001',5,180.00,'VIP90','east-01'),
(11,90010,'bad-sku',-1,42.00,NULL,'tmp');

INSERT INTO ops.pay_evt VALUES
(50001,90001,'2026-05-01 09:32:11','card',300.00,true),
(50002,90002,'2026-05-01 14:24:02','wallet',178.20,true),
(50003,90003,'2026-05-02 10:09:53','bank',1160.00,true),
(50004,90004,'2026-05-03 16:45:10','wallet',429.70,true),
(50005,90005,'2026-05-04 11:01:10','card',88.00,false),
(50006,90006,'2026-05-04 21:09:00','wallet',239.90,true),
(50007,90007,'2026-05-05 08:18:00','card',620.00,true),
(50008,90008,'2026-05-06 12:48:00','card',58.00,true),
(50009,90009,'2026-05-07 19:03:00','wallet',909.00,true);

INSERT INTO ops.refund_x VALUES
(7001,90004,'2026-05-05 10:00:00',120.00,'quality','done'),
(7002,90005,'2026-05-04 11:30:00',88.00,'cancelled','closed'),
(7003,90009,'2026-05-09 17:00:00',99.00,'late ship','DONE'),
(7004,90010,'2026-05-09 08:00:00',42.00,'bad sku','pending');

INSERT INTO stg.x_mkt_cmp VALUES
('c-ad-may','May performance ads','ad','zoe',5000.00,'2026-05-01','2026-05-31'),
('c-push-7','App push week 1','push','han',800.00,'2026-05-01','2026-05-07'),
('c-kol-42','Creator batch 42','kol','mei',2300.00,'2026-05-03','2026-05-12'),
('seo-base','SEO always on','seo','sys',0.00,'2026-01-01','2026-12-31');

INSERT INTO ops.traffic_daily VALUES
('2026-05-01','ad','c-ad-may',1800,120,320.00),
('2026-05-01','seo','seo-base',900,60,0.00),
('2026-05-03','push','c-push-7',4000,310,90.00),
('2026-05-04','kol','c-kol-42',2100,180,280.00),
('2026-05-05','ad','c-ad-may',2300,160,410.00),
('2026-05-07','push','c-push-7',3700,290,85.00);

INSERT INTO shadow.tmp_2024_x VALUES ('a','orphan','2024x'),('b','legacy','n/a');
INSERT INTO shadow.job_audit_evt VALUES ('load_order','2026-05-08 01:00:00','warn','late rows'),('load_sku','2026-05-08 01:03:00','info','ok');
