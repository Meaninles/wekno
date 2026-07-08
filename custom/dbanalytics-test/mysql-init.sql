CREATE DATABASE IF NOT EXISTS retail_ops CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE retail_ops;

CREATE TABLE cust (
  cust_id BIGINT PRIMARY KEY COMMENT 'customer id',
  nm VARCHAR(120),
  seg VARCHAR(40) COMMENT 'customer segment, not normalized',
  reg_cd VARCHAR(20),
  joined_dt DATE,
  vip_flg TINYINT(1),
  raw_note TEXT
) COMMENT='customer master copied from CRM';

CREATE TABLE ord_hdr (
  oid BIGINT PRIMARY KEY,
  cust_id BIGINT,
  odt DATETIME,
  stat_cd VARCHAR(40),
  gross_amt DECIMAL(12,2),
  disc_amt DECIMAL(12,2),
  pay_amt DECIMAL(12,2) COMMENT 'actual paid amount',
  ch VARCHAR(40),
  src_cd VARCHAR(40)
) COMMENT='order header facts';

CREATE TABLE ord_ln (
  line_id BIGINT PRIMARY KEY,
  oid BIGINT,
  sku VARCHAR(80),
  qty INT COMMENT 'quantity, correction rows can be negative',
  unit_px DECIMAL(12,2),
  promo_cd VARCHAR(60),
  wh VARCHAR(60)
);

CREATE TABLE sku_dim (
  sku VARCHAR(80) PRIMARY KEY,
  pnm VARCHAR(180),
  cat1 VARCHAR(80),
  cat2 VARCHAR(80),
  brand VARCHAR(80),
  cost_amt DECIMAL(12,2)
) COMMENT='SKU dimension';

CREATE TABLE pay_evt (
  pay_id BIGINT PRIMARY KEY,
  oid BIGINT,
  pay_ts DATETIME,
  method VARCHAR(40),
  amt DECIMAL(12,2),
  ok_flg TINYINT(1)
);

CREATE TABLE refund_x (
  rid BIGINT PRIMARY KEY,
  oid BIGINT,
  r_dt DATETIME,
  amt DECIMAL(12,2),
  why VARCHAR(180),
  state VARCHAR(40)
) COMMENT='refund events with inconsistent states';

CREATE TABLE traffic_daily (
  dt DATE,
  ch VARCHAR(40),
  camp_id VARCHAR(80),
  uv INT,
  clk INT,
  spend DECIMAL(12,2)
);

CREATE TABLE x_mkt_cmp (
  camp_id VARCHAR(80) PRIMARY KEY,
  nm VARCHAR(180),
  chn VARCHAR(40),
  owner VARCHAR(80),
  budget_amt DECIMAL(12,2) COMMENT 'planned media budget',
  start_dt DATE,
  end_dt DATE
);

CREATE TABLE tmp_2024_x (
  k VARCHAR(60),
  v TEXT,
  dt VARCHAR(40)
);

CREATE TABLE job_audit_evt (
  job_nm VARCHAR(80),
  ts DATETIME,
  lvl VARCHAR(20),
  msg TEXT
);

INSERT INTO cust VALUES
(1001,'Ava Chen','vip','E','2025-01-06',1,'prefers email'),
(1002,'Noah Li','new','N','2025-02-18',0,NULL),
(1003,'Mia Zhang','wholesale','S','2024-11-02',0,'contract price'),
(1004,'Kai Wang','VIP','E','2025-03-15',1,'duplicate phone?'),
(1005,'Lina Xu','normal','W','2025-01-28',0,''),
(1006,'Ben Q','trial','N','2025-04-01',0,'imported from event');

INSERT INTO sku_dim VALUES
('sku-001','Aurora Coffee Beans','grocery','coffee','Northstar',38.20),
('sku-002','Orbit Tea Pack','grocery','tea','Orbit',18.10),
('sku-003','Pulse Blender','appliance','kitchen','Pulse',210.00),
('sku-004','Zen Bottle 500ml','outdoor','drinkware','Zen',42.50),
('sku-005','Nova Headset','electronics','audio','Nova',159.90),
('bad-sku','unknown item',NULL,NULL,NULL,NULL);

INSERT INTO ord_hdr VALUES
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

INSERT INTO ord_ln VALUES
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

INSERT INTO pay_evt VALUES
(50001,90001,'2026-05-01 09:32:11','card',300.00,1),
(50002,90002,'2026-05-01 14:24:02','wallet',178.20,1),
(50003,90003,'2026-05-02 10:09:53','bank',1160.00,1),
(50004,90004,'2026-05-03 16:45:10','wallet',429.70,1),
(50005,90005,'2026-05-04 11:01:10','card',88.00,0),
(50006,90006,'2026-05-04 21:09:00','wallet',239.90,1),
(50007,90007,'2026-05-05 08:18:00','card',620.00,1),
(50008,90008,'2026-05-06 12:48:00','card',58.00,1),
(50009,90009,'2026-05-07 19:03:00','wallet',909.00,1);

INSERT INTO refund_x VALUES
(7001,90004,'2026-05-05 10:00:00',120.00,'quality','done'),
(7002,90005,'2026-05-04 11:30:00',88.00,'cancelled','closed'),
(7003,90009,'2026-05-09 17:00:00',99.00,'late ship','DONE'),
(7004,90010,'2026-05-09 08:00:00',42.00,'bad sku','pending');

INSERT INTO x_mkt_cmp VALUES
('c-ad-may','May performance ads','ad','zoe',5000.00,'2026-05-01','2026-05-31'),
('c-push-7','App push week 1','push','han',800.00,'2026-05-01','2026-05-07'),
('c-kol-42','Creator batch 42','kol','mei',2300.00,'2026-05-03','2026-05-12'),
('seo-base','SEO always on','seo','sys',0.00,'2026-01-01','2026-12-31');

INSERT INTO traffic_daily VALUES
('2026-05-01','ad','c-ad-may',1800,120,320.00),
('2026-05-01','seo','seo-base',900,60,0.00),
('2026-05-03','push','c-push-7',4000,310,90.00),
('2026-05-04','kol','c-kol-42',2100,180,280.00),
('2026-05-05','ad','c-ad-may',2300,160,410.00),
('2026-05-07','push','c-push-7',3700,290,85.00);

INSERT INTO tmp_2024_x VALUES ('a','orphan','2024x'),('b','legacy','n/a');
INSERT INTO job_audit_evt VALUES ('load_order','2026-05-08 01:00:00','warn','late rows'),('load_sku','2026-05-08 01:03:00','info','ok');

CREATE USER IF NOT EXISTS 'analytics_reader'@'%' IDENTIFIED BY 'analytics_pwd';
ALTER USER 'analytics_reader'@'%' IDENTIFIED BY 'analytics_pwd';
REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'analytics_reader'@'%';
GRANT SELECT, SHOW VIEW ON `retail_ops`.* TO 'analytics_reader'@'%';
FLUSH PRIVILEGES;
