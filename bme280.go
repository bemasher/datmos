package main

import (
	"math"
)

type BME280 struct {
	tfine int32

	calibrated bool

	T1 uint16
	T2 int16
	T3 int16

	P1 uint16
	P2 int16
	P3 int16
	P4 int16
	P5 int16
	P6 int16
	P7 int16
	P8 int16
	P9 int16

	H1 uint8
	H2 int16
	H3 uint8
	H4 int16
	H5 int16
	H6 int8

	temperature float64
	humidity    float64
	pressure    float64
}

func (b *BME280) Cal(cal []byte) {
	tpCal := cal[:26]
	hCal := cal[26:]

	b.T1 = uint16(tpCal[1])<<8 | uint16(tpCal[0])
	b.T2 = int16(tpCal[3])<<8 | int16(tpCal[2])
	b.T3 = int16(tpCal[5])<<8 | int16(tpCal[4])

	b.P1 = uint16(tpCal[7])<<8 | uint16(tpCal[6])
	b.P2 = int16(tpCal[9])<<8 | int16(tpCal[8])
	b.P3 = int16(tpCal[11])<<8 | int16(tpCal[10])
	b.P4 = int16(tpCal[13])<<8 | int16(tpCal[12])
	b.P5 = int16(tpCal[15])<<8 | int16(tpCal[14])
	b.P6 = int16(tpCal[17])<<8 | int16(tpCal[16])
	b.P7 = int16(tpCal[19])<<8 | int16(tpCal[18])
	b.P8 = int16(tpCal[21])<<8 | int16(tpCal[20])
	b.P9 = int16(tpCal[23])<<8 | int16(tpCal[22])

	b.H1 = tpCal[25]
	b.H2 = int16(hCal[1])<<8 | int16(hCal[0])
	b.H3 = hCal[2]
	b.H4 = int16(hCal[3])<<4 | int16(hCal[4]&0x0F)
	b.H5 = int16(hCal[4]&0xF0)>>4 | (int16(hCal[5]) << 4)
	b.H6 = int8(hCal[6])

	b.calibrated = true
}

func (b *BME280) Update(buf []byte) {
	b.temperature = b.Temperature(buf)
	b.humidity = b.Humidity(buf)
	b.pressure = b.Pressure(buf)
}

func (b *BME280) Temperature(buf []byte) float64 {
	// Uncompensated temperature
	uct := float64(uint32(buf[3])<<12 | uint32(buf[4])<<4 | uint32(buf[5]>>4))

	t1 := float64(b.T1)
	t2 := float64(b.T2)
	t3 := float64(b.T3)

	v1 := (uct/16384.0 - t1/1024.0) * t2
	v2 := ((uct/131072.0 - t1/8192.0) * (uct/131072.0 - t1/8192.0)) * t3

	b.tfine = int32(v1 + v2)

	return CtoF((v1 + v2) / 5120.0)
}

func (b *BME280) Humidity(buf []byte) float64 {
	// Uncompensated humidity
	uch := float64(uint16(buf[6])<<8 | uint16(buf[7]))

	h1 := float64(b.H1)
	h2 := float64(b.H2)
	h3 := float64(b.H3)
	h4 := float64(b.H4)
	h5 := float64(b.H5)
	h6 := float64(b.H6)

	h := float64(b.tfine) - 76800
	h = (uch - (h4*64.0 + h5/16384.8*h)) * (h2 / 65536.0 * (1.0 + h6/67108864.0*h*(1.0+h3/67108864.0*h)))
	h = h * (1.0 - h1*h/524288.0)

	switch {
	case h > 100:
		return 100
	case h < 0:
		return 0
	}

	return h
}

func (b *BME280) Pressure(buf []byte) float64 {
	// Uncompensated pressure
	ucp := float64(uint32(buf[0])<<12 | uint32(buf[1])<<4 | uint32(buf[2])>>4)

	p1 := float64(b.P1)
	p2 := float64(b.P2)
	p3 := float64(b.P3)
	p4 := float64(b.P4)
	p5 := float64(b.P5)
	p6 := float64(b.P6)
	p7 := float64(b.P7)
	p8 := float64(b.P8)
	p9 := float64(b.P9)

	v1 := 0.5*float64(b.tfine) - 64000.0
	v2 := v1*v1*p6/32768.0 + v1*p5*2
	v2 = v2/4 + p4*65536
	v1 = (p3*v1*v1/524288.0 + p2*v1) / 524288.0
	v1 = (1.0 + v1/32768.0) * p1
	if v1 == 0 {
		return 0
	}

	p := 1048576.0 - ucp
	p = ((p - v2/4096.0) * 6250.0) / v1
	v1 = p9 * p * p / 2147483648.0
	v2 = p * p8 / 32768.0
	return (p + (v1+v2+p7)/16.0) / 100.0
}

func CtoF(t float64) float64 {
	return t*1.8 + 32
}

func FtoC(t float64) float64 {
	return (t - 32) / 1.8
}

const (
	ß = 17.62
	λ = 243.12
)

func DewPoint(t, rh float64) float64 {
	t = FtoC(t)
	rh /= 100.0

	ßt := ß * t
	λt := λ + t
	rhLn := math.Log(rh)
	α := rhLn + ßt/λt

	return CtoF((λ * α) / (ß - α))
}
