create or replace TABLE STORES (
	STORE_ID NUMBER(38,0) NOT NULL autoincrement,
	STORE_NAME VARCHAR(255) NOT NULL,
	PHONE VARCHAR(25),
	EMAIL VARCHAR(255),
	STREET VARCHAR(255),
	CITY VARCHAR(255),
	STATE VARCHAR(10),
	ZIP_CODE VARCHAR(5),
	primary key (STORE_ID)
);
